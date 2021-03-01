/* Firecracker-task-driver is a task driver for Hashicorp's nomad that allows
 * to create microvms using AWS Firecracker vmm
 * Copyright (C) 2019  Carlos Neira cneirabustos@gmail.com
 *
 * This file is part of Firecracker-task-driver.
 *
 * Foobar is free software: you can redistribute it and/or modify
 * it under the terms of the GNU General Public License as published by
 * the Free Software Foundation, either version 2 of the License, or
 * (at your option) any later version.
 *
 * Firecracker-task-driver is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU General Public License for more details.
 *
 * You should have received a copy of the GNU General Public License
 * along with Firecracker-task-driver. If not, see <http://www.gnu.org/licenses/>.
 */

package firevm

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/containerd/console"
	firecracker "github.com/firecracker-microvm/firecracker-go-sdk"
	hclog "github.com/hashicorp/go-hclog"
	"github.com/hashicorp/nomad/client/lib/fifo"
	"github.com/hashicorp/nomad/plugins/drivers"
	log "github.com/sirupsen/logrus"
)

const (
	// executableMask is the mask needed to check whether or not a file's
	// permissions are executable.
	executableMask = 0111

	// containerMonitorIntv is the interval at which the driver checks if the
	// firecracker micro-vm is still running
	containerMonitorIntv = 2 * time.Second
	defaultbootoptions   = " console=ttyS0 reboot=k panic=1 pci=off nomodules"
)

func taskConfig2FirecrackerOpts(taskConfig TaskConfig, cfg *drivers.TaskConfig) (*options, error) {
	opts := newOptions()

	if len(taskConfig.KernelImage) > 0 {
		opts.FcKernelImage = taskConfig.KernelImage
	} else {
		opts.FcKernelImage = filepath.Join(cfg.AllocDir, cfg.Name) + "/vmlinux"
	}

	if len(taskConfig.BootDisk) > 0 {
		opts.FcRootDrivePath = taskConfig.BootDisk
	} else {
		opts.FcRootDrivePath = filepath.Join(cfg.AllocDir, cfg.Name) + "/rootfs.ext4"
	}

	if len(taskConfig.Disks) > 0 {
		opts.FcAdditionalDrives = taskConfig.Disks
	}

	if len(taskConfig.BootOptions) > 0 {
		opts.FcKernelCmdLine = taskConfig.BootOptions + defaultbootoptions
	} else {
		opts.FcKernelCmdLine = defaultbootoptions
	}

	if len(taskConfig.Nic.Ip) > 0 {
		opts.FcNicConfig = taskConfig.Nic
	}
	if len(taskConfig.Network) > 0 {
		opts.FcNetworkName = taskConfig.Network
	}

	if len(taskConfig.Log) > 0 {
		opts.FcFifoLogFile = taskConfig.Log
		opts.Debug = true
		opts.FcLogLevel = "Debug"
	}

	if cfg.Resources.NomadResources.Cpu.CpuShares > 100 {
		opts.FcCPUCount = cfg.Resources.NomadResources.Cpu.CpuShares / 100
	} else {
		opts.FcCPUCount = 1
	}
	opts.FcCPUTemplate = taskConfig.Cputype
	opts.FcDisableHt = taskConfig.DisableHt

	if cfg.Resources.NomadResources.Memory.MemoryMB > 0 {
		opts.FcMemSz = cfg.Resources.NomadResources.Memory.MemoryMB
	} else {
		opts.FcMemSz = 300
	}
	opts.FcBinary = taskConfig.Firecracker

	return opts, nil
}

type vminfo struct {
	Machine      *firecracker.Machine
	SocketPath   string
	StdOutSerial string
	StdErrSerial string
	Info         Instance_info
}

type Instance_info struct {
	AllocId      string
	Ip           string
	SocketPath   string
	StdOutSerial string
	StdErrSerial string
	Pid          string
	VMID         string
}

func (d *Driver) recoverContainer(ctx context.Context, cfg *drivers.TaskConfig, taskConfig TaskConfig) (*vminfo, error) {
	opts, _ := taskConfig2FirecrackerOpts(taskConfig, cfg)
	fcCfg, err := opts.getFirecrackerConfig(cfg.TaskDir().LocalDir)
	if err != nil {
		log.Errorf("Error: %s", err)
		return nil, err
	}

	d.logger.Info("Starting firecracker", "driver_initialize_container", hclog.Fmt("%v+", opts))
	logger := log.New()

	if opts.Debug {
		log.SetLevel(log.DebugLevel)
		logger.SetLevel(log.DebugLevel)
	}

	vmmCtx, vmmCancel := context.WithCancel(ctx)
	defer vmmCancel()

	machineOpts := []firecracker.Opt{
		firecracker.WithLogger(log.NewEntry(logger)),
	}

	fcenv := os.Getenv("FIRECRACKER_BIN")
	var firecrackerBinary string
	if len(opts.FcBinary) > 0 {
		firecrackerBinary = opts.FcBinary
	} else if len(fcenv) > 0 {
		firecrackerBinary = fcenv
	} else {
		firecrackerBinary = "/usr/bin/firecracker"
	}

	finfo, err := os.Stat(firecrackerBinary)
	if os.IsNotExist(err) {
		return nil, fmt.Errorf("Binary %q does not exist: %v", firecrackerBinary, err)
	}
	if err != nil {
		return nil, fmt.Errorf("Failed to stat binary, %q: %v", firecrackerBinary, err)
	}
	if finfo.IsDir() {
		return nil, fmt.Errorf("Binary, %q, is a directory", firecrackerBinary)
	} else if finfo.Mode()&executableMask == 0 {
		return nil, fmt.Errorf("Binary, %q, is not executable. Check permissions of binary", firecrackerBinary)
	}

	stateFile := fmt.Sprintf("%s/state.json", cfg.AllocDir)
	log, err := os.ReadFile(stateFile)
	if err != nil {
		d.logger.Error("Unable to open stat file %s to recover firecracker task: %s", stateFile, err.Error())
		return nil, err
	}

	info := Instance_info{}
	err = json.Unmarshal(log, &info)
	if err != nil {
		d.logger.Error("Unable to read contet of file %s to recover firecracker task: %s", stateFile, err.Error())
		return nil, err
	}
	fcCfg.VMID = info.VMID

	cmd := firecracker.VMCommandBuilder{}.
		WithBin(firecrackerBinary).
		WithSocketPath(fcCfg.SocketPath).
		WithStdin(nil).
		WithStdout(nil).
		WithStderr(nil).
		Build(ctx)

	go d.logging(ctx, cfg.StdoutPath, cfg.StderrPath, info.StdOutSerial, info.StdOutSerial)

	machineOpts = append(machineOpts, firecracker.WithProcessRunner(cmd))
	m, err := firecracker.NewMachine(vmmCtx, fcCfg, machineOpts...)
	if err != nil {
		return nil, fmt.Errorf("Failed creating machine: %v", err)
	}

	if err := m.Start(vmmCtx); err != firecracker.ErrAlreadyStarted {
		return nil, fmt.Errorf("Failed to recover machine: %v", err)
	}

	if opts.validMetadata != nil {
		m.SetMetadata(vmmCtx, opts.validMetadata)
	}

	return &vminfo{Machine: m, SocketPath: m.Cfg.SocketPath, Info: info}, nil
}

func (d *Driver) initializeContainer(ctx context.Context, cfg *drivers.TaskConfig, taskConfig TaskConfig) (*vminfo, error) {
	opts, _ := taskConfig2FirecrackerOpts(taskConfig, cfg)
	fcCfg, err := opts.getFirecrackerConfig(cfg.TaskDir().LocalDir)
	if err != nil {
		log.Errorf("Error: %s", err)
		return nil, err
	}

	d.logger.Info("Starting firecracker", "driver_initialize_container", hclog.Fmt("%v+", opts))
	logger := log.New()

	if opts.Debug {
		log.SetLevel(log.DebugLevel)
		logger.SetLevel(log.DebugLevel)
	}

	vmmCtx, vmmCancel := context.WithCancel(ctx)
	defer vmmCancel()

	machineOpts := []firecracker.Opt{
		firecracker.WithLogger(log.NewEntry(logger)),
	}

	fcenv := os.Getenv("FIRECRACKER_BIN")
	var firecrackerBinary string
	if len(opts.FcBinary) > 0 {
		firecrackerBinary = opts.FcBinary
	} else if len(fcenv) > 0 {
		firecrackerBinary = fcenv
	} else {
		firecrackerBinary = "/usr/bin/firecracker"
	}

	finfo, err := os.Stat(firecrackerBinary)
	if os.IsNotExist(err) {
		return nil, fmt.Errorf("Binary %q does not exist: %v", firecrackerBinary, err)
	}
	if err != nil {
		return nil, fmt.Errorf("Failed to stat binary, %q: %v", firecrackerBinary, err)
	}
	if finfo.IsDir() {
		return nil, fmt.Errorf("Binary, %q, is a directory", firecrackerBinary)
	} else if finfo.Mode()&executableMask == 0 {
		return nil, fmt.Errorf("Binary, %q, is not executable. Check permissions of binary", firecrackerBinary)
	}

	otty, ottyName, err := console.NewPty()
	if err != nil {
		return nil, fmt.Errorf("Could not create serial console  %v+", err)
	}

	etty, ettyName, err := console.NewPty()
	if err != nil {
		return nil, fmt.Errorf("Could not create serial console  %v+", err)
	}

	go d.logging(ctx, cfg.StdoutPath, cfg.StderrPath, ottyName, ettyName)

	cmd := firecracker.VMCommandBuilder{}.
		WithBin(firecrackerBinary).
		WithSocketPath(fcCfg.SocketPath).
		WithStdin(nil).
		WithStdout(otty).
		WithStderr(etty).
		Build(ctx)

	machineOpts = append(machineOpts, firecracker.WithProcessRunner(cmd))

	m, err := firecracker.NewMachine(vmmCtx, fcCfg, machineOpts...)
	if err != nil {
		return nil, fmt.Errorf("Failed creating machine: %v", err)
	}

	if err := m.Start(vmmCtx); err != nil {
		return nil, fmt.Errorf("Failed to start machine: %v", err)
	}

	if opts.validMetadata != nil {
		m.SetMetadata(vmmCtx, opts.validMetadata)
	}

	pid, errpid := m.PID()
	if errpid != nil {
		return nil, fmt.Errorf("Failed getting pid for machine: %v", errpid)
	}
	var ip string
	if len(opts.FcNetworkName) > 0 {
		ip = fcCfg.NetworkInterfaces[0].StaticConfiguration.IPConfiguration.IPAddr.String()
	} else {
		ip = "No network chosen"
	}

	info := Instance_info{
		SocketPath:   m.Cfg.SocketPath,
		AllocId:      cfg.AllocID,
		Ip:           ip,
		Pid:          strconv.Itoa(pid),
		StdOutSerial: ottyName,
		StdErrSerial: ettyName,
		VMID:         m.Cfg.VMID,
	}

	f, _ := json.MarshalIndent(info, "", " ")
	stateFile := fmt.Sprintf("%s/state.json", cfg.AllocDir)
	err = os.WriteFile(stateFile, f, 0644)
	if err != nil {
		return nil, fmt.Errorf("Failed creating info file=%s err=%v", stateFile, err)
	}
	d.logger.Info("Writing to", "driver_initialize_container", hclog.Fmt("%v+", stateFile))

	return &vminfo{Machine: m, SocketPath: m.Cfg.SocketPath, Info: info}, nil
}

func (d *Driver) logging(ctx context.Context, stdoutPath string, stderrPath string, ottyName string, ettyName string) error {
	ottyFile, err := os.OpenFile(ottyName, os.O_RDONLY|syscall.O_NOCTTY, 0)
	if err != nil {
		d.logger.Error("Unable to open tty %s to redirect firecracker stdout: %s", ottyName, err.Error())
		return err
	}
	defer ottyFile.Close()

	stdoutFifo, err := fifo.OpenWriter(stdoutPath)
	if err != nil {
		d.logger.Error("Unable to open logfile %s to redirect firecracker stdout: %s", stdoutPath, err.Error())
		return err
	}
	defer stdoutFifo.Close()

	ettyFile, err := os.OpenFile(ettyName, os.O_RDONLY|syscall.O_NOCTTY, 0)
	if err != nil {
		d.logger.Error("Unable to open tty %s to redirect firecracker stderr: %s", ettyFile, err.Error())
		return err
	}
	defer ettyFile.Close()

	stderrFifo, err := fifo.OpenWriter(stderrPath)
	if err != nil {
		d.logger.Error("Unable to open logfile %s to redirect firecracker stderr: %s", stderrPath, err.Error())
		return err
	}
	defer stderrFifo.Close()

	go io.Copy(stdoutFifo, ottyFile)
	go io.Copy(stderrFifo, ettyFile)

	for {
		select {
		case <-ctx.Done():
			return nil
		}
	}
}
