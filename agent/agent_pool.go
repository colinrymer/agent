package agent

import (
	"fmt"
	"os"
	"runtime"
	"time"

	"github.com/buildkite/agent/api"
	"github.com/buildkite/agent/logger"
	"github.com/buildkite/agent/retry"
	"github.com/buildkite/agent/signalwatcher"
	"github.com/buildkite/agent/system"
)

type AgentPool struct {
	APIClient          *api.Client
	Token              string
	ConfigFilePath     string
	Name               string
	Priority           string
	MetaData           []string
	MetaDataEC2        bool
	MetaDataEC2Tags    bool
	MetaDataGCP        bool
	Endpoint           string
	AgentConfiguration *AgentConfiguration
}

func (r *AgentPool) Start() error {
	// Show the welcome banner and config options used
	r.ShowBanner()

	// Create the agent registration API Client
	r.APIClient = APIClient{Endpoint: r.Endpoint, Token: r.Token}.Create()

	// Create the agent template. We use pass this template to the register
	// call, at which point we get back a real agent.
	template := r.CreateAgentTemplate()

	logger.Info("Registering agent with Buildkite...")

	// Register the agent
	registered, err := r.RegisterAgent(template)
	if err != nil {
		logger.Fatal("%s", err)
	}

	logger.Info("Successfully registered agent \"%s\" with meta-data %s", registered.Name, registered.MetaData)

	logger.Debug("Ping interval: %ds", registered.PingInterval)
	logger.Debug("Heartbeat interval: %ds", registered.HearbeatInterval)

	// Now that we have a registered agent, we can connect it to the API,
	// and start running jobs.
	worker := AgentWorker{Agent: registered, AgentConfiguration: r.AgentConfiguration, Endpoint: r.Endpoint}.Create()

	logger.Info("Connecting to Buildkite...")
	if err := worker.Connect(); err != nil {
		logger.Fatal("%s", err)
	}

	logger.Info("Agent successfully connected")
	logger.Info("You can press Ctrl-C to stop the agent")
	logger.Info("Waiting for work...")

	// Start a signalwatcher so we can monitor signals and handle shutdowns
	signalwatcher.Watch(func(sig signalwatcher.Signal) {
		if sig == signalwatcher.QUIT {
			logger.Debug("Received signal `%s`", sig.String())
			worker.Stop(false)
		} else if sig == signalwatcher.TERM || sig == signalwatcher.INT {
			logger.Debug("Received signal `%s`", sig.String())
			worker.Stop(true)
		} else {
			logger.Debug("Ignoring signal `%s`", sig.String())
		}
	})

	// Starts the agent worker. This will block until the agent has
	// finished or is stopped.
	if err := worker.Start(); err != nil {
		logger.Fatal("%s", err)
	}

	// Now that the agent has stopped, we can disconnect it
	logger.Info("Disconnecting %s...", worker.Agent.Name)
	worker.Disconnect()

	return nil
}

// Takes the options passed to the CLI, and creates an api.Agent record that
// will be sent to the Buildkite Agent API for registration.
func (r *AgentPool) CreateAgentTemplate() *api.Agent {
	agent := &api.Agent{
		Name:              r.Name,
		Priority:          r.Priority,
		MetaData:          r.MetaData,
		ScriptEvalEnabled: r.AgentConfiguration.CommandEval,
		Version:           Version(),
		Build:             BuildVersion(),
		PID:               os.Getpid(),
		Arch:              runtime.GOARCH,
	}

	// Attempt to add the EC2 meta-data
	if r.MetaDataEC2 {
		tags, err := EC2MetaData{}.Get()
		if err != nil {
			// Don't blow up if we can't find them, just show a nasty error.
			logger.Error(fmt.Sprintf("Failed to fetch EC2 meta-data: %s", err.Error()))
		} else {
			for tag, value := range tags {
				agent.MetaData = append(agent.MetaData, fmt.Sprintf("%s=%s", tag, value))
			}
		}
	}

	// Attempt to add the EC2 tags
	if r.MetaDataEC2Tags {
		tags, err := EC2Tags{}.Get()
		if err != nil {
			// Don't blow up if we can't find them, just show a nasty error.
			logger.Error(fmt.Sprintf("Failed to find EC2 Tags: %s", err.Error()))
		} else {
			for tag, value := range tags {
				agent.MetaData = append(agent.MetaData, fmt.Sprintf("%s=%s", tag, value))
			}
		}
	}

	// Attempt to add the Google Cloud meta-data
	if r.MetaDataGCP {
		tags, err := GCPMetaData{}.Get()
		if err != nil {
			// Don't blow up if we can't find them, just show a nasty error.
			logger.Error(fmt.Sprintf("Failed to fetch Google Cloud meta-data: %s", err.Error()))
		} else {
			for tag, value := range tags {
				agent.MetaData = append(agent.MetaData, fmt.Sprintf("%s=%s", tag, value))
			}
		}
	}

	var err error

	// Add the hostname
	agent.Hostname, err = os.Hostname()
	if err != nil {
		logger.Warn("Failed to find hostname: %s", err)
	}

	// Add the OS dump
	agent.OS, err = system.VersionDump()
	if err != nil {
		logger.Warn("Failed to find OS information: %s", err)
	}

	return agent
}

// Takes the agent template and returns a registered agent. The registered
// agent includes the Access Token used to communicate with the Buildkite Agent
// API
func (r *AgentPool) RegisterAgent(agent *api.Agent) (*api.Agent, error) {
	var registered *api.Agent
	var err error
	var resp *api.Response

	register := func(s *retry.Stats) error {
		registered, resp, err = r.APIClient.Agents.Register(agent)
		if err != nil {
			if resp != nil && resp.StatusCode == 401 {
				logger.Warn("Buildkite rejected the registration (%s)", err)
				s.Break()
			} else {
				logger.Warn("%s (%s)", err, s)
			}
		}

		return err
	}

	// Try to register, retrying every 10 seconds for a maximum of 30 attempts (5 minutes)
	err = retry.Do(register, &retry.Config{Maximum: 30, Interval: 10 * time.Second})

	return registered, err
}

// Shows the welcome banner and the configuration options used when starting
// this agent.
func (r *AgentPool) ShowBanner() {
	welcomeMessage :=
		"\n" +
			"%s  _           _ _     _ _    _ _                                _\n" +
			" | |         (_) |   | | |  (_) |                              | |\n" +
			" | |__  _   _ _| | __| | | ___| |_ ___    __ _  __ _  ___ _ __ | |_\n" +
			" | '_ \\| | | | | |/ _` | |/ / | __/ _ \\  / _` |/ _` |/ _ \\ '_ \\| __|\n" +
			" | |_) | |_| | | | (_| |   <| | ||  __/ | (_| | (_| |  __/ | | | |_\n" +
			" |_.__/ \\__,_|_|_|\\__,_|_|\\_\\_|\\__\\___|  \\__,_|\\__, |\\___|_| |_|\\__|\n" +
			"                                                __/ |\n" +
			" http://buildkite.com/agent                    |___/\n%s\n"

	if logger.ColorsEnabled() {
		fmt.Fprintf(logger.OutputPipe(), welcomeMessage, "\x1b[32m", "\x1b[0m")
	} else {
		fmt.Fprintf(logger.OutputPipe(), welcomeMessage, "", "")
	}

	logger.Notice("Starting buildkite-agent v%s with PID: %s", Version(), fmt.Sprintf("%d", os.Getpid()))
	logger.Notice("The agent source code can be found here: https://github.com/buildkite/agent")
	logger.Notice("For questions and support, email us at: hello@buildkite.com")

	if r.ConfigFilePath != "" {
		logger.Info("Configuration loaded from: %s", r.ConfigFilePath)
	}

	logger.Debug("Bootstrap command: %s", r.AgentConfiguration.BootstrapScript)
	logger.Debug("Build path: %s", r.AgentConfiguration.BuildPath)
	logger.Debug("Hooks directory: %s", r.AgentConfiguration.HooksPath)
	logger.Debug("Plugins directory: %s", r.AgentConfiguration.PluginsPath)

	if !r.AgentConfiguration.SSHFingerprintVerification {
		logger.Debug("Automatic SSH fingerprint verification has been disabled")
	}

	if !r.AgentConfiguration.CommandEval {
		logger.Debug("Evaluating console commands has been disabled")
	}

	if !r.AgentConfiguration.RunInPty {
		logger.Debug("Running builds within a pseudoterminal (PTY) has been disabled")
	}
}
