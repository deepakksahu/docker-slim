package container

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/docker-slim/docker-slim/internal/app/master/config"
	"github.com/docker-slim/docker-slim/internal/app/master/docker/dockerhost"
	"github.com/docker-slim/docker-slim/internal/app/master/inspectors/container/ipc"
	"github.com/docker-slim/docker-slim/internal/app/master/inspectors/image"
	"github.com/docker-slim/docker-slim/internal/app/master/security/apparmor"
	"github.com/docker-slim/docker-slim/internal/app/master/security/seccomp"
	"github.com/docker-slim/docker-slim/pkg/ipc/command"
	"github.com/docker-slim/docker-slim/pkg/report"
	"github.com/docker-slim/docker-slim/pkg/utils/errutils"
	"github.com/docker-slim/docker-slim/pkg/utils/fsutils"

	log "github.com/Sirupsen/logrus"
	dockerapi "github.com/cloudimmunity/go-dockerclientx"
)

// IpcErrRecvTimeoutStr - an IPC receive timeout error
const IpcErrRecvTimeoutStr = "receive time out"

const (
	SensorBinPath     = "/opt/dockerslim/bin/sensor"
	ContainerNamePat  = "dockerslimk_%v_%v"
	ArtifactsDir      = "artifacts"
	SensorBinLocal    = "docker-slim-sensor"
	ArtifactsMountPat = "%s:/opt/dockerslim/artifacts"
	SensorMountPat    = "%s:/opt/dockerslim/bin/sensor:ro"
	CmdPortDefault    = "65501/tcp"
	EvtPortDefault    = "65502/tcp"
	LabelName         = "dockerslim"
)

// Inspector is a container execution inspector
type Inspector struct {
	ContainerInfo     *dockerapi.Container
	ContainerID       string
	ContainerName     string
	FatContainerCmd   []string
	LocalVolumePath   string
	CmdPort           dockerapi.Port
	EvtPort           dockerapi.Port
	DockerHostIP      string
	ImageInspector    *image.Inspector
	APIClient         *dockerapi.Client
	Overrides         *config.ContainerOverrides
	Links             []string
	EtcHostsMaps      []string
	DnsServers        []string
	DnsSearchDomains  []string
	ShowContainerLogs bool
	VolumeMounts      map[string]config.VolumeMount
	ExcludePaths      map[string]bool
	IncludePaths      map[string]bool
	DoDebug           bool
}

func pathMapKeys(m map[string]bool) []string {
	if len(m) == 0 {
		return nil
	}

	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}

	return keys
}

// NewInspector creates a new container execution inspector
func NewInspector(client *dockerapi.Client,
	imageInspector *image.Inspector,
	localVolumePath string,
	overrides *config.ContainerOverrides,
	links []string,
	etcHostsMaps []string,
	dnsServers []string,
	dnsSearchDomains []string,
	showContainerLogs bool,
	volumeMounts map[string]config.VolumeMount,
	excludePaths map[string]bool,
	includePaths map[string]bool,
	doDebug bool) (*Inspector, error) {

	inspector := &Inspector{
		LocalVolumePath:   localVolumePath,
		CmdPort:           CmdPortDefault,
		EvtPort:           EvtPortDefault,
		ImageInspector:    imageInspector,
		APIClient:         client,
		Overrides:         overrides,
		Links:             links,
		EtcHostsMaps:      etcHostsMaps,
		DnsServers:        dnsServers,
		DnsSearchDomains:  dnsSearchDomains,
		ShowContainerLogs: showContainerLogs,
		VolumeMounts:      volumeMounts,
		ExcludePaths:      excludePaths,
		IncludePaths:      includePaths,
		DoDebug:           doDebug,
	}

	if overrides != nil && ((len(overrides.Entrypoint) > 0) || overrides.ClearEntrypoint) {
		log.Debugf("overriding Entrypoint %+v => %+v (%v)",
			imageInspector.ImageInfo.Config.Entrypoint, overrides.Entrypoint, overrides.ClearEntrypoint)
		if len(overrides.Entrypoint) > 0 {
			inspector.FatContainerCmd = append(inspector.FatContainerCmd, overrides.Entrypoint...)
		}

	} else if len(imageInspector.ImageInfo.Config.Entrypoint) > 0 {
		inspector.FatContainerCmd = append(inspector.FatContainerCmd, imageInspector.ImageInfo.Config.Entrypoint...)
	}

	if overrides != nil && ((len(overrides.Cmd) > 0) || overrides.ClearCmd) {
		log.Debugf("overriding Cmd %+v => %+v (%v)",
			imageInspector.ImageInfo.Config.Cmd, overrides.Cmd, overrides.ClearCmd)
		if len(overrides.Cmd) > 0 {
			inspector.FatContainerCmd = append(inspector.FatContainerCmd, overrides.Cmd...)
		}

	} else if len(imageInspector.ImageInfo.Config.Cmd) > 0 {
		inspector.FatContainerCmd = append(inspector.FatContainerCmd, imageInspector.ImageInfo.Config.Cmd...)
	}

	return inspector, nil
}

// RunContainer starts the container inspector instance execution
func (i *Inspector) RunContainer() error {
	artifactsPath := filepath.Join(i.LocalVolumePath, ArtifactsDir)
	sensorPath := filepath.Join(fsutils.ExeDir(), SensorBinLocal)

	artifactsMountInfo := fmt.Sprintf(ArtifactsMountPat, artifactsPath)
	sensorMountInfo := fmt.Sprintf(SensorMountPat, sensorPath)

	var volumeBinds []string
	for _, volumeMount := range i.VolumeMounts {
		mountInfo := fmt.Sprintf("%s:%s:%s", volumeMount.Source, volumeMount.Destination, volumeMount.Options)
		volumeBinds = append(volumeBinds, mountInfo)
	}

	volumeBinds = append(volumeBinds, artifactsMountInfo)
	volumeBinds = append(volumeBinds, sensorMountInfo)

	var containerCmd []string
	if i.DoDebug {
		containerCmd = append(containerCmd, "-d")
	}

	i.ContainerName = fmt.Sprintf(ContainerNamePat, os.Getpid(), time.Now().UTC().Format("20060102150405"))

	containerOptions := dockerapi.CreateContainerOptions{
		Name: i.ContainerName,
		Config: &dockerapi.Config{
			Image: i.ImageInspector.ImageRef,
			//ExposedPorts: map[dockerapi.Port]struct{}{
			//	i.CmdPort: {},
			//	i.EvtPort: {},
			//},
			Entrypoint: []string{SensorBinPath},
			Cmd:        containerCmd,
			Env:        i.Overrides.Env,
			Labels:     map[string]string{"type": LabelName},
			Hostname:   i.Overrides.Hostname,
		},
		HostConfig: &dockerapi.HostConfig{
			Binds:           volumeBinds,
			PublishAllPorts: true,
			CapAdd:          []string{"SYS_ADMIN"},
			Privileged:      true,
		},
	}

	commsExposedPorts := map[dockerapi.Port]struct{}{
		i.CmdPort: {},
		i.EvtPort: {},
	}

	if len(i.Overrides.ExposedPorts) > 0 {
		containerOptions.Config.ExposedPorts = i.Overrides.ExposedPorts
		for k, v := range commsExposedPorts {
			if _, ok := containerOptions.Config.ExposedPorts[k]; ok {
				log.Warnf("RunContainer: comms port conflict => %v", k)
			}

			containerOptions.Config.ExposedPorts[k] = v
		}
		log.Debugf("RunContainer: Config.ExposedPorts => %#v", containerOptions.Config.ExposedPorts)
	} else {
		containerOptions.Config.ExposedPorts = commsExposedPorts
		log.Debug("RunContainer: default exposed ports => %#v", containerOptions.Config.ExposedPorts)
	}

	if i.Overrides.Network != "" {
		containerOptions.HostConfig.NetworkMode = i.Overrides.Network
		log.Debugf("RunContainer: HostConfig.NetworkMode => %v", i.Overrides.Network)
	}

	// adding this separately for better visibility...
	if len(i.Links) > 0 {
		containerOptions.HostConfig.Links = i.Links
		log.Debugf("RunContainer: HostConfig.Links => %v", i.Links)
	}

	if len(i.EtcHostsMaps) > 0 {
		containerOptions.HostConfig.ExtraHosts = i.EtcHostsMaps
		log.Debugf("RunContainer: HostConfig.ExtraHosts => %v", i.EtcHostsMaps)
	}

	if len(i.DnsServers) > 0 {
		containerOptions.HostConfig.DNS = i.DnsServers //for newer versions of Docker
		containerOptions.Config.DNS = i.DnsServers     //for older versions of Docker
		log.Debugf("RunContainer: HostConfig.DNS/Config.DNS => %v", i.DnsServers)
	}

	if len(i.DnsSearchDomains) > 0 {
		containerOptions.HostConfig.DNSSearch = i.DnsSearchDomains
		log.Debugf("RunContainer: HostConfig.DNSSearch => %v", i.DnsSearchDomains)
	}

	containerInfo, err := i.APIClient.CreateContainer(containerOptions)
	if err != nil {
		return err
	}

	i.ContainerID = containerInfo.ID
	log.Infoln("RunContainer: created container =>", i.ContainerID)

	if err := i.APIClient.StartContainer(i.ContainerID, nil); err != nil {
		return err
	}

	if i.ContainerInfo, err = i.APIClient.InspectContainer(i.ContainerID); err != nil {
		return err
	}

	errutils.FailWhen(i.ContainerInfo.NetworkSettings == nil, "docker-slim: error => no network info")
	errutils.FailWhen(len(i.ContainerInfo.NetworkSettings.Ports) < len(commsExposedPorts), "docker-slim: error => missing comms ports")
	log.Debugf("RunContainer: container NetworkSettings.Ports => %#v", i.ContainerInfo.NetworkSettings.Ports)

	if err = i.initContainerChannels(); err != nil {
		return err
	}

	cmd := &command.StartMonitor{
		AppName: i.FatContainerCmd[0],
	}

	if len(i.FatContainerCmd) > 1 {
		cmd.AppArgs = i.FatContainerCmd[1:]
	}

	if len(i.ExcludePaths) > 0 {
		cmd.Excludes = pathMapKeys(i.ExcludePaths)
	}

	if len(i.IncludePaths) > 0 {
		cmd.Includes = pathMapKeys(i.IncludePaths)
	}

	_, err = ipc.SendContainerCmd(cmd)
	return err
}

func (i *Inspector) showContainerLogs() {
	var outData bytes.Buffer
	outw := bufio.NewWriter(&outData)
	var errData bytes.Buffer
	errw := bufio.NewWriter(&errData)

	log.Debug("getting container logs => ", i.ContainerID)
	logsOptions := dockerapi.LogsOptions{
		Container:    i.ContainerID,
		OutputStream: outw,
		ErrorStream:  errw,
		Stdout:       true,
		Stderr:       true,
	}

	err := i.APIClient.Logs(logsOptions)
	if err != nil {
		log.Infof("error getting container logs => %v - %v", i.ContainerID, err)
	} else {
		outw.Flush()
		errw.Flush()
		fmt.Println("docker-slim: container stdout:")
		outData.WriteTo(os.Stdout)
		fmt.Println("docker-slim: container stderr:")
		errData.WriteTo(os.Stdout)
		fmt.Println("docker-slim: end of container logs =============")
	}
}

// ShutdownContainer terminates the container inspector instance execution
func (i *Inspector) ShutdownContainer() error {
	i.shutdownContainerChannels()

	if i.ShowContainerLogs {
		i.showContainerLogs()
	}

	err := i.APIClient.StopContainer(i.ContainerID, 9)

	if _, ok := err.(*dockerapi.ContainerNotRunning); ok {
		log.Info("can't stop the docker-slim container (container is not running)...")

		//show container logs if they aren't shown yet
		if !i.ShowContainerLogs {
			i.showContainerLogs()
		}

	} else {
		errutils.WarnOn(err)
	}

	removeOption := dockerapi.RemoveContainerOptions{
		ID:            i.ContainerID,
		RemoveVolumes: true,
		Force:         true,
	}
	_ = i.APIClient.RemoveContainer(removeOption)
	return nil
}

// FinishMonitoring ends the target container monitoring activities
func (i *Inspector) FinishMonitoring() {
	cmdResponse, err := ipc.SendContainerCmd(&command.StopMonitor{})
	errutils.WarnOn(err)
	//_ = cmdResponse
	log.Debugf("'stop' monitor response => '%v'", cmdResponse)

	log.Info("waiting for the container to finish its work...")

	//for now there's only one event ("done")
	//getEvt() should timeout in two minutes (todo: pick a good timeout)
	evt, err := ipc.GetContainerEvt()
	log.Debugf("sensor event => '%v'", evt)

	//don't want to expose mangos here... mangos.ErrRecvTimeout = errors.New("receive time out")
	if err != nil && err.Error() == IpcErrRecvTimeoutStr {
		log.Info("timeout waiting for the docker-slim container to finish its work...")
		return
	}

	errutils.WarnOn(err)
	_ = evt
	log.Debugf("sensor event => '%v'", evt)

	cmdResponse, err = ipc.SendContainerCmd(&command.ShutdownSensor{})
	errutils.WarnOn(err)
	log.Debugf("'shutdown' sensor response => '%v'", cmdResponse)
}

func (i *Inspector) initContainerChannels() error {
	/*
		NOTE: not using IPC for now... (future option for regular Docker deployments)
		ipcLocation := filepath.Join(localVolumePath,"ipc")
		_, err = os.Stat(ipcLocation)
		if os.IsNotExist(err) {
			os.MkdirAll(ipcLocation, 0777)
			_, err = os.Stat(ipcLocation)
			errutils.FailOn(err)
		}
	*/

	cmdPortBindings := i.ContainerInfo.NetworkSettings.Ports[i.CmdPort]
	evtPortBindings := i.ContainerInfo.NetworkSettings.Ports[i.EvtPort]
	i.DockerHostIP = dockerhost.GetIP()

	if err := ipc.InitContainerChannels(i.DockerHostIP, cmdPortBindings[0].HostPort, evtPortBindings[0].HostPort); err != nil {
		return err
	}

	return nil
}

func (i *Inspector) shutdownContainerChannels() {
	ipc.ShutdownContainerChannels()
}

// HasCollectedData returns true if any data was produced monitoring the target container
func (i *Inspector) HasCollectedData() bool {
	return fsutils.Exists(filepath.Join(i.ImageInspector.ArtifactLocation, report.DefaultContainerReportFileName))
}

// ProcessCollectedData performs post-processing on the collected container data
func (i *Inspector) ProcessCollectedData() error {
	log.Info("generating AppArmor profile...")
	err := apparmor.GenProfile(i.ImageInspector.ArtifactLocation, i.ImageInspector.AppArmorProfileName)
	if err != nil {
		return err
	}

	return seccomp.GenProfile(i.ImageInspector.ArtifactLocation, i.ImageInspector.SeccompProfileName)
}
