package uninstall

import (
	"context"
	"fmt"
	"os"

	"github.com/integrii/flaggy"
	"go.uber.org/zap"
	"k8s.io/utils/strings/slices"

	"github.com/aws/eks-hybrid/internal/cleanup"
	"github.com/aws/eks-hybrid/internal/cli"
	"github.com/aws/eks-hybrid/internal/cni"
	"github.com/aws/eks-hybrid/internal/daemon"
	"github.com/aws/eks-hybrid/internal/flows"
	"github.com/aws/eks-hybrid/internal/kubelet"
	"github.com/aws/eks-hybrid/internal/logger"
	"github.com/aws/eks-hybrid/internal/node"
	"github.com/aws/eks-hybrid/internal/packagemanager"
	"github.com/aws/eks-hybrid/internal/tracker"
)

const (
	skipPodPreflightCheck  = "pod-validation"
	skipNodePreflightCheck = "node-validation"
)

const uninstallHelpText = `Examples:
  # Uninstall all components
  nodeadm uninstall

  # Uninstall all components and skip pod-validation and node-validation pre-flight validation
  nodeadm uninstall --skip node-validation,pod-validation

Documentation:
  https://docs.aws.amazon.com/eks/latest/userguide/hybrid-nodes-nodeadm.html#_uninstall`

func NewCommand() cli.Command {
	cmd := command{}

	fc := flaggy.NewSubcommand("uninstall")
	fc.Description = "Uninstall components installed using the install sub-command"
	fc.AdditionalHelpAppend = uninstallHelpText
	fc.StringSlice(&cmd.skipPhases, "s", "skip", "Phases of uninstall to skip. Allowed values: [pod-validation, node-validation].")
	fc.Bool(&cmd.force, "f", "force", "Force delete additional directories that might contain leftovers from the node process. WARNING: This will delete all contents in default Kubernetes and CNI directories (/var/lib/cni, /etc/cni/net.d, etc). Do not use this flag if you store your own data in these locations.")
	cmd.flaggy = fc

	return &cmd
}

type command struct {
	flaggy     *flaggy.Subcommand
	skipPhases []string
	force      bool
}

func (c *command) Flaggy() *flaggy.Subcommand {
	return c.flaggy
}

func (c *command) Run(log *zap.Logger, opts *cli.GlobalOptions) error {
	ctx := context.Background()
	ctx = logger.NewContext(ctx, log)

	root, err := cli.IsRunningAsRoot()
	if err != nil {
		return err
	}
	if !root {
		return cli.ErrMustRunAsRoot
	}

	log.Info("Loading installed components")
	installed, err := tracker.GetInstalledArtifacts()
	if err != nil && os.IsNotExist(err) {
		log.Info("Nodeadm components are already uninstalled")
		return nil
	} else if err != nil {
		return err
	}

	log.Info("Creating daemon manager..")
	daemonManager, err := daemon.NewDaemonManager()
	if err != nil {
		return err
	}
	defer daemonManager.Close()

	if installed.Artifacts.Kubelet {
		kubeletStatus, err := daemonManager.GetDaemonStatus(kubelet.KubeletDaemonName)
		if err != nil {
			return err
		}
		if kubeletStatus == daemon.DaemonStatusRunning {
			if !slices.Contains(c.skipPhases, skipPodPreflightCheck) {
				log.Info("Validating if node has been drained...")
				if drained, err := node.IsDrained(ctx); err != nil {
					return fmt.Errorf("validating if node has been drained: %w", err)
				} else if !drained {
					return fmt.Errorf("only static pods and pods controlled by daemon-sets can be running on the node. Please move pods " +
						"to different node or use --skip pod-validation")
				}
			}
			if !slices.Contains(c.skipPhases, skipNodePreflightCheck) {
				log.Info("Validating if node has been marked unschedulable...")
				if err := node.IsUnscheduled(ctx); err != nil {
					return fmt.Errorf("please drain or cordon node to mark it unschedulable or use --skip node-validation: %w", err)
				}
			}
		}
	}

	log.Info("Creating package manager...")
	containerdSource := installed.Artifacts.Containerd
	log.Info("Configuring package manager with", zap.Reflect("containerd source", string(containerdSource)))
	packageManager, err := packagemanager.New(containerdSource, log)
	if err != nil {
		return err
	}

	uninstaller := &flows.Uninstaller{
		Artifacts:      installed.Artifacts,
		DaemonManager:  daemonManager,
		PackageManager: packageManager,
		Logger:         log,
		CNIUninstall:   cni.Uninstall,
	}

	if err := uninstaller.Run(ctx); err != nil {
		return err
	}

	if c.force {
		log.Info("Force mode enabled, cleaning up additional directories...")
		cleanupManager := cleanup.New(log)
		if err := cleanupManager.Cleanup(); err != nil {
			return fmt.Errorf("cleaning up additional directories: %w", err)
		}
	}

	return nil
}
