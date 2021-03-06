package commands

import (
	"fmt"

	"github.com/docker-slim/docker-slim/internal/app/master/config"
	"github.com/docker-slim/docker-slim/internal/app/master/docker/dockerclient"
	"github.com/docker-slim/docker-slim/internal/app/master/inspectors/image"
	"github.com/docker-slim/docker-slim/internal/app/master/version"
	"github.com/docker-slim/docker-slim/pkg/report"
	"github.com/docker-slim/docker-slim/pkg/utils/errutils"
	"github.com/docker-slim/docker-slim/pkg/utils/fsutils"

	log "github.com/Sirupsen/logrus"
	"github.com/dustin/go-humanize"
)

// OnInfo implements the 'info' docker-slim command
func OnInfo(
	cmdReportLocation string,
	doDebug bool,
	statePath string,
	clientConfig *config.DockerClient,
	imageRef string) {
	logger := log.WithFields(log.Fields{"app": "docker-slim", "command": "info"})

	cmdReport := report.NewInfoCommand(cmdReportLocation)
	cmdReport.State = report.CmdStateStarted
	cmdReport.OriginalImage = imageRef

	fmt.Println("docker-slim[info]: state=started")
	fmt.Printf("docker-slim[info]: info=params target=%v\n", imageRef)

	client := dockerclient.New(clientConfig)

	if doDebug {
		version.Print(client)
	}

	imageInspector, err := image.NewInspector(client, imageRef)
	errutils.FailOn(err)

	if imageInspector.NoImage() {
		fmt.Println("docker-slim[info]: target image not found -", imageRef)
		fmt.Println("docker-slim[info]: state=exited")
		return
	}

	logger.Info("inspecting 'fat' image metadata...")
	err = imageInspector.Inspect()
	errutils.FailOn(err)

	_, artifactLocation := fsutils.PrepareStateDirs(statePath, imageInspector.ImageInfo.ID)
	imageInspector.ArtifactLocation = artifactLocation

	fmt.Printf("docker-slim[info]: info=image id=%v size.bytes=%v size.human=%v\n",
		imageInspector.ImageInfo.ID,
		imageInspector.ImageInfo.VirtualSize,
		humanize.Bytes(uint64(imageInspector.ImageInfo.VirtualSize)))

	logger.Info("processing 'fat' image info...")
	err = imageInspector.ProcessCollectedData()
	errutils.FailOn(err)

	fmt.Println("docker-slim[info]: state=completed")
	cmdReport.State = report.CmdStateCompleted

	fmt.Println("docker-slim[info]: state=done")
	cmdReport.State = report.CmdStateDone
	cmdReport.Save()
}
