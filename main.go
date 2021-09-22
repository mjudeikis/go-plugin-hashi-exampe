package main

import (
	"context"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/davecgh/go-spew/spew"
	hclog "github.com/hashicorp/go-hclog"
	plugin "github.com/hashicorp/go-plugin"
	"github.com/hashicorp/nomad/client/allocdir"
	"github.com/hashicorp/nomad/client/config"
	"github.com/hashicorp/nomad/client/logmon"
	"github.com/hashicorp/nomad/client/taskenv"
	"github.com/hashicorp/nomad/drivers/docker"
	"github.com/hashicorp/nomad/helper/uuid"
	"github.com/hashicorp/nomad/nomad/mock"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/hashicorp/nomad/plugins/base"
	"github.com/hashicorp/nomad/plugins/drivers"
)

var (
	// busyboxLongRunningCmd is a busybox command that runs indefinitely, and
	// ideally responds to SIGINT/SIGTERM.  Sadly, busybox:1.29.3 /bin/sleep doesn't.
	busyboxLongRunningCmd = []string{"nc", "-l", "-p", "3000", "127.0.0.1"}
)

var (
	basicResources = &drivers.Resources{
		NomadResources: &structs.AllocatedTaskResources{
			Memory: structs.AllocatedMemoryResources{
				MemoryMB: 256,
			},
			Cpu: structs.AllocatedCpuResources{
				CpuShares: 250,
			},
		},
		LinuxResources: &drivers.LinuxResources{
			CPUShares:        512,
			MemoryLimitBytes: 256 * 1024 * 1024,
		},
	}
)

type DriverHarness struct {
	drivers.DriverPlugin
	logger hclog.Logger
	impl   drivers.DriverPlugin
}

func main() {
	ctx := context.Background()

	logger := hclog.NewInterceptLogger(&hclog.LoggerOptions{
		Name:       "agent",
		Level:      hclog.LevelFromString("debug"),
		Output:     os.Stdout,
		JSONFormat: true,
	})

	d := docker.NewDockerDriver(ctx, logger)
	pd := drivers.NewDriverPlugin(d, logger)

	client := plugin.NewClient(&plugin.ClientConfig{
		HandshakeConfig: base.Handshake,
		Plugins: plugin.PluginSet{
			base.PluginTypeDriver: pd,
			base.PluginTypeBase:   &base.PluginBase{Impl: d},
			"logmon":              logmon.NewPlugin(logmon.NewLogMon(logger.Named("logmon"))),
		},

		AllowedProtocols: []plugin.Protocol{
			plugin.ProtocolGRPC,
		},

		Cmd: exec.Command("./plugins/docker"),
	})
	defer client.Kill()

	rpcClient, err := client.Client()
	if err != nil {
		log.Fatal(err)
	}

	raw, err := rpcClient.Dispense(base.PluginTypeDriver)
	if err != nil {
		log.Fatal(err)
	}

	dClient := raw.(drivers.DriverPlugin)

	dh := DriverHarness{
		logger:       logger,
		DriverPlugin: dClient,
		impl:         d,
	}

	var data []byte
	baseConfig := &base.Config{PluginConfig: data}
	err = dClient.SetConfig(baseConfig)
	if err != nil {
		log.Fatal(err)
	}

	// try
	taskCfg := newTaskConfig("", busyboxLongRunningCmd)
	task := &drivers.TaskConfig{
		ID:        uuid.Generate(),
		Name:      "nc-demo",
		AllocID:   uuid.Generate(),
		Resources: basicResources,
	}

	cleanup, err := dh.MkAllocDir(task, true)
	if err != nil {
		log.Fatal(err)
	}
	defer cleanup()

	task.EncodeConcreteDriverConfig(&taskCfg)

	th, _, err := dClient.StartTask(task)
	if err != nil {
		log.Fatal(err)
	}

	defer func() {
		err := dClient.StopTask(task.ID, time.Second, "SIGINT")
		if err != nil {
			log.Fatal(err)
		}
		time.Sleep(time.Second * 5)
	}()

	for {
		spew.Dump(th.State)

		time.Sleep(1 * time.Second)
	}

}

func newTaskConfig(variant string, command []string) docker.TaskConfig {
	// busyboxImageID is the ID stored in busybox.tar
	busyboxImageID := "busybox:1.29.3"

	image := busyboxImageID
	loadImage := "busybox.tar"
	if variant != "" {
		image = fmt.Sprintf("%s-%s", busyboxImageID, variant)
		loadImage = fmt.Sprintf("busybox_%s.tar", variant)
	}

	return docker.TaskConfig{
		Image:            image,
		ImagePullTimeout: "5m",
		LoadImage:        loadImage,
		Command:          command[0],
		Args:             command[1:],
	}
}

// MkAllocDir creates a temporary directory and allocdir structure.
// If enableLogs is set to true a logmon instance will be started to write logs
// to the LogDir of the task
// A cleanup func is returned and should be deferred so as to not leak dirs
// between tests.
func (h *DriverHarness) MkAllocDir(t *drivers.TaskConfig, enableLogs bool) (func(), error) {
	dir, err := ioutil.TempDir("", "nomad_driver_harness-")
	if err != nil {
		return nil, err
	}
	t.AllocDir = dir

	allocDir := allocdir.NewAllocDir(h.logger, dir)
	err = allocDir.Build()
	if err != nil {
		return nil, err
	}

	taskDir := allocDir.NewTaskDir(t.Name)

	caps, err := h.Capabilities()
	if err != nil {
		return nil, err
	}

	fsi := caps.FSIsolation
	err = taskDir.Build(fsi == drivers.FSIsolationChroot, config.DefaultChrootEnv)
	if err != nil {
		return nil, err
	}

	task := &structs.Task{
		Name: t.Name,
		Env:  t.Env,
	}

	// Create the mock allocation
	alloc := mock.Alloc()
	if t.Resources != nil {
		alloc.AllocatedResources.Tasks[task.Name] = t.Resources.NomadResources
	}

	taskBuilder := taskenv.NewBuilder(mock.Node(), alloc, task, "global")
	SetEnvvars(taskBuilder, fsi, taskDir, config.DefaultConfig())

	taskEnv := taskBuilder.Build()
	if t.Env == nil {
		t.Env = taskEnv.Map()
	} else {
		for k, v := range taskEnv.Map() {
			if _, ok := t.Env[k]; !ok {
				t.Env[k] = v
			}
		}
	}

	//logmon
	if enableLogs {
		lm := logmon.NewLogMon(h.logger.Named("logmon"))
		if runtime.GOOS == "windows" {
			id := uuid.Generate()[:8]
			t.StdoutPath = fmt.Sprintf("//./pipe/%s-%s.stdout", t.Name, id)
			t.StderrPath = fmt.Sprintf("//./pipe/%s-%s.stderr", t.Name, id)
		} else {
			t.StdoutPath = filepath.Join(taskDir.LogDir, fmt.Sprintf(".%s.stdout.fifo", t.Name))
			t.StderrPath = filepath.Join(taskDir.LogDir, fmt.Sprintf(".%s.stderr.fifo", t.Name))
		}
		err = lm.Start(&logmon.LogConfig{
			LogDir:        taskDir.LogDir,
			StdoutLogFile: fmt.Sprintf("%s.stdout", t.Name),
			StderrLogFile: fmt.Sprintf("%s.stderr", t.Name),
			StdoutFifo:    t.StdoutPath,
			StderrFifo:    t.StderrPath,
			MaxFiles:      10,
			MaxFileSizeMB: 10,
		})
		if err != nil {
			return nil, err
		}

		return func() {
			lm.Stop()
			allocDir.Destroy()
		}, nil
	}

	return func() {
		allocDir.Destroy()
	}, nil
}

// SetEnvvars sets path and host env vars depending on the FS isolation used.
func SetEnvvars(envBuilder *taskenv.Builder, fsi drivers.FSIsolation, taskDir *allocdir.TaskDir, conf *config.Config) {

	envBuilder.SetClientTaskRoot(taskDir.Dir)
	envBuilder.SetClientSharedAllocDir(taskDir.SharedAllocDir)
	envBuilder.SetClientTaskLocalDir(taskDir.LocalDir)
	envBuilder.SetClientTaskSecretsDir(taskDir.SecretsDir)

	// Set driver-specific environment variables
	switch fsi {
	case drivers.FSIsolationNone:
		// Use host paths
		envBuilder.SetAllocDir(taskDir.SharedAllocDir)
		envBuilder.SetTaskLocalDir(taskDir.LocalDir)
		envBuilder.SetSecretsDir(taskDir.SecretsDir)
	default:
		// filesystem isolation; use container paths
		envBuilder.SetAllocDir(allocdir.SharedAllocContainerPath)
		envBuilder.SetTaskLocalDir(allocdir.TaskLocalContainerPath)
		envBuilder.SetSecretsDir(allocdir.TaskSecretsContainerPath)
	}

	// Set the host environment variables for non-image based drivers
	if fsi != drivers.FSIsolationImage {
		// COMPAT(1.0) using inclusive language, blacklist is kept for backward compatibility.
		filter := strings.Split(conf.ReadAlternativeDefault(
			[]string{"env.denylist", "env.blacklist"},
			config.DefaultEnvDenylist,
		), ",")
		envBuilder.SetHostEnvvars(filter)
	}
}
