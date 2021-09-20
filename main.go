package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"

	hclog "github.com/hashicorp/go-hclog"
	plugin "github.com/hashicorp/go-plugin"
	"github.com/hashicorp/nomad/client/logmon"
	"github.com/hashicorp/nomad/drivers/docker"
	"github.com/hashicorp/nomad/helper/uuid"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/hashicorp/nomad/plugins/base"
	"github.com/hashicorp/nomad/plugins/drivers"
)

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

	//cfg := map[string]interface{}{
	//	"gc": map[string]interface{}{
	//		"image":       false,
	//		"image_delay": "1s",
	//	},
	//}

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

	fmt.Println(client.NegotiatedVersion())

	rpcClient, err := client.Client()
	if err != nil {
		log.Fatal(err)
	}

	raw, err := rpcClient.Dispense(base.PluginTypeDriver)
	if err != nil {
		log.Fatal(err)
	}

	dClient := raw.(drivers.DriverPlugin)
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

	task.EncodeConcreteDriverConfig(&taskCfg)

	_, _, err = dClient.StartTask(task)
	if err != nil {
		log.Fatal(err)
	}
}

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

var (
	// busyboxLongRunningCmd is a busybox command that runs indefinitely, and
	// ideally responds to SIGINT/SIGTERM.  Sadly, busybox:1.29.3 /bin/sleep doesn't.
	busyboxLongRunningCmd = []string{"nc", "-l", "-p", "3000", "127.0.0.1"}
)
