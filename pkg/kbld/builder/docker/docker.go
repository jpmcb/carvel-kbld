// Copyright 2020 VMware, Inc.
// SPDX-License-Identifier: Apache-2.0

package image

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	regname "github.com/google/go-containerregistry/pkg/name"
	ctlb "github.com/vmware-tanzu/carvel-kbld/pkg/kbld/builder"
	ctllog "github.com/vmware-tanzu/carvel-kbld/pkg/kbld/logger"
)

type Docker struct {
	logger ctllog.Logger
}

type DockerBuildOpts struct {
	// https://docs.docker.com/engine/reference/commandline/build/
	Target     *string
	Pull       *bool
	NoCache    *bool
	File       *string
	Buildkit   *bool
	RawOptions *[]string
}

type DockerTmpRef struct {
	val string
}

func NewDockerTmpRef(val string) DockerTmpRef {
	return DockerTmpRef{val}
}

func (r DockerTmpRef) AsString() string { return r.val }

type DockerImageDigest struct {
	val string
}

func (r DockerImageDigest) AsString() string { return r.val }

func NewDocker(logger ctllog.Logger) Docker {
	return Docker{logger}
}

func (d Docker) Build(image, directory string, opts DockerBuildOpts) (DockerTmpRef, error) {
	err := d.ensureDirectory(directory)
	if err != nil {
		return DockerTmpRef{}, err
	}

	tb := ctlb.TagBuilder{}

	randPrefix50, err := tb.RandomStr50()
	if err != nil {
		return DockerTmpRef{}, fmt.Errorf("Generating tmp image suffix: %s", err)
	}

	tmpRef := DockerTmpRef{"kbld:" + tb.CheckTagLen128(fmt.Sprintf(
		"%s-%s",
		randPrefix50,
		tb.TrimStr(tb.CleanStr(image), 50),
	))}

	prefixedLogger := d.logger.NewPrefixedWriter(image + " | ")

	prefixedLogger.Write([]byte(fmt.Sprintf("starting build (using Docker): %s -> %s\n", directory, tmpRef.AsString())))
	defer prefixedLogger.Write([]byte("finished build (using Docker)\n"))

	{
		var stdoutBuf, stderrBuf bytes.Buffer

		cmdArgs := []string{"build"}

		if opts.Target != nil {
			cmdArgs = append(cmdArgs, "--target", *opts.Target)
		}
		if opts.Pull != nil && *opts.Pull {
			cmdArgs = append(cmdArgs, "--pull")
		}
		if opts.NoCache != nil && *opts.NoCache {
			cmdArgs = append(cmdArgs, "--no-cache")
		}
		if opts.File != nil {
			// Since docker command is executed with cwd of directory,
			// Dockerfile path doesnt need to be joined with it
			cmdArgs = append(cmdArgs, "--file", *opts.File)
		}
		if opts.RawOptions != nil {
			cmdArgs = append(cmdArgs, *opts.RawOptions...)
		}

		cmdArgs = append(cmdArgs, "--tag", tmpRef.AsString(), ".")

		cmd := exec.Command("docker", cmdArgs...)
		cmd.Dir = directory
		cmd.Stdout = io.MultiWriter(&stdoutBuf, prefixedLogger)
		cmd.Stderr = io.MultiWriter(&stderrBuf, prefixedLogger)

		if opts.Buildkit != nil {
			cmd.Env = append(os.Environ(), "DOCKER_BUILDKIT=1")
		}

		err := cmd.Run()
		if err != nil {
			prefixedLogger.Write([]byte(fmt.Sprintf("error: %s\n", err)))
			return DockerTmpRef{}, err
		}
	}

	inspectData, err := d.inspect(tmpRef.AsString())
	if err != nil {
		prefixedLogger.Write([]byte(fmt.Sprintf("inspect error: %s\n", err)))
		return DockerTmpRef{}, err
	}

	return d.RetagStable(tmpRef, image, inspectData.ID, prefixedLogger)
}

func (d Docker) RetagStable(tmpRef DockerTmpRef, image, imageID string,
	prefixedLogger *ctllog.PrefixWriter) (DockerTmpRef, error) {

	tb := ctlb.TagBuilder{}

	// Retag image with its sha256 to produce exact image ref if nothing has changed.
	// Seems that Docker doesn't like `kbld@sha256:...` format for local images.
	// Image hint at the beginning for easier sorting.
	stableTmpRef := DockerTmpRef{"kbld:" + tb.CheckTagLen128(fmt.Sprintf(
		"%s-%s",
		tb.TrimStr(tb.CleanStr(image), 50),
		tb.CheckLen(tb.CleanStr(imageID), 72),
	))}

	{
		var stdoutBuf, stderrBuf bytes.Buffer

		cmd := exec.Command("docker", "tag", tmpRef.AsString(), stableTmpRef.AsString())
		cmd.Stdout = io.MultiWriter(&stdoutBuf, prefixedLogger)
		cmd.Stderr = io.MultiWriter(&stderrBuf, prefixedLogger)

		err := cmd.Run()
		if err != nil {
			prefixedLogger.Write([]byte(fmt.Sprintf("tag error: %s\n", err)))
			return DockerTmpRef{}, err
		}
	}

	// Remove temporary tag to be nice to `docker images` output.
	// (No point in "untagging" digest reference)
	if !strings.HasPrefix(tmpRef.AsString(), "sha256:") {
		var stdoutBuf, stderrBuf bytes.Buffer

		cmd := exec.Command("docker", "rmi", tmpRef.AsString())
		cmd.Stdout = io.MultiWriter(&stdoutBuf, prefixedLogger)
		cmd.Stderr = io.MultiWriter(&stderrBuf, prefixedLogger)

		err := cmd.Run()
		if err != nil {
			prefixedLogger.Write([]byte(fmt.Sprintf("untag error: %s\n", err)))
			return DockerTmpRef{}, err
		}
	}

	return stableTmpRef, nil
}

func (d Docker) Push(tmpRef DockerTmpRef, imageDst string) (DockerImageDigest, error) {
	prefixedLogger := d.logger.NewPrefixedWriter(imageDst + " | ")

	tb := ctlb.TagBuilder{}

	// Generate random tag for pushed image.
	// TODO we are technically polluting registry with new tags.
	// Unfortunately we do not know digest upfront so cannot use kbld-sha256-... format.
	imageDstTagged, err := regname.NewTag(imageDst, regname.WeakValidation)
	if err == nil {
		randSuffix, err := tb.RandomStr50()
		if err != nil {
			return DockerImageDigest{}, fmt.Errorf("Generating image dst suffix: %s", err)
		}

		imageDstTag := fmt.Sprintf("kbld-%s", randSuffix)

		imageDstTagged, err = regname.NewTag(imageDst+":"+imageDstTag, regname.WeakValidation)
		if err != nil {
			return DockerImageDigest{}, fmt.Errorf("Generating image dst tag '%s': %s", imageDst, err)
		}
	}

	imageDst = imageDstTagged.Name()

	prefixedLogger.Write([]byte(fmt.Sprintf("starting push (using Docker): %s -> %s\n", tmpRef.AsString(), imageDst)))
	defer prefixedLogger.Write([]byte("finished push (using Docker)\n"))

	prevInspectData, err := d.inspect(tmpRef.AsString())
	if err != nil {
		prefixedLogger.Write([]byte(fmt.Sprintf("inspect error: %s\n", err)))
		return DockerImageDigest{}, err
	}

	{
		var stdoutBuf, stderrBuf bytes.Buffer

		cmd := exec.Command("docker", "tag", tmpRef.AsString(), imageDst)
		cmd.Stdout = io.MultiWriter(&stdoutBuf, prefixedLogger)
		cmd.Stderr = io.MultiWriter(&stderrBuf, prefixedLogger)

		err := cmd.Run()
		if err != nil {
			prefixedLogger.Write([]byte(fmt.Sprintf("tag error: %s\n", err)))
			return DockerImageDigest{}, err
		}
	}

	{
		var stdoutBuf, stderrBuf bytes.Buffer

		cmd := exec.Command("docker", "push", imageDst)
		cmd.Stdout = io.MultiWriter(&stdoutBuf, prefixedLogger)
		cmd.Stderr = io.MultiWriter(&stderrBuf, prefixedLogger)

		err := cmd.Run()
		if err != nil {
			prefixedLogger.Write([]byte(fmt.Sprintf("push error: %s\n", err)))
			return DockerImageDigest{}, err
		}
	}

	currInspectData, err := d.inspect(imageDst)
	if err != nil {
		prefixedLogger.Write([]byte(fmt.Sprintf("inspect error: %s\n", err)))
		return DockerImageDigest{}, err
	}

	// Try to detect if image we should be pushing isnt the one we ended up pushing
	// given that its theoretically possible concurrent Docker commands
	// may have retagged in the middle of the process.
	if prevInspectData.ID != currInspectData.ID {
		prefixedLogger.Write([]byte(fmt.Sprintf("push race error: %s\n", err)))
		return DockerImageDigest{}, err
	}

	return d.determineRepoDigest(currInspectData, prefixedLogger)
}

func (d Docker) ensureDirectory(directory string) error {
	stat, err := os.Stat(directory)
	if err != nil {
		return fmt.Errorf("Checking if path '%s' is a directory: %s", directory, err)
	}

	// Provide explicit directory check error message because otherwise docker CLI
	// outputs confusing msg 'error: fork/exec /usr/local/bin/docker: not a directory'
	if !stat.IsDir() {
		return fmt.Errorf("Expected path '%s' to be a directory, but was not", directory)
	}

	return nil
}

func (d Docker) determineRepoDigest(inspectData dockerInspectData,
	prefixedLogger *ctllog.PrefixWriter) (DockerImageDigest, error) {

	if len(inspectData.RepoDigests) == 0 {
		prefixedLogger.Write([]byte("missing repo digest\n"))
		return DockerImageDigest{}, fmt.Errorf("Expected to find at least one repo digest")
	}

	digestStrs := map[string]struct{}{}

	for _, rd := range inspectData.RepoDigests {
		nameWithDigest, err := regname.NewDigest(rd, regname.WeakValidation)
		if err != nil {
			return DockerImageDigest{}, fmt.Errorf("Extracting reference digest from '%s': %s", rd, err)
		}
		digestStrs[nameWithDigest.DigestStr()] = struct{}{}
	}

	if len(digestStrs) != 1 {
		prefixedLogger.Write([]byte("repo digests mismatch\n"))
		return DockerImageDigest{}, fmt.Errorf("Expected to find same repo digest, but found %#v", inspectData.RepoDigests)
	}

	for digest := range digestStrs {
		return DockerImageDigest{digest}, nil
	}

	panic("unreachable")
}

type dockerInspectData struct {
	ID          string
	RepoDigests []string
}

func (d Docker) inspect(ref string) (dockerInspectData, error) {
	var stdoutBuf, stderrBuf bytes.Buffer

	cmd := exec.Command("docker", "inspect", ref)
	cmd.Stdout = &stdoutBuf
	cmd.Stderr = &stderrBuf

	err := cmd.Run()
	if err != nil {
		return dockerInspectData{}, err
	}

	var data []dockerInspectData

	err = json.Unmarshal(stdoutBuf.Bytes(), &data)
	if err != nil {
		return dockerInspectData{}, err
	}

	if len(data) != 1 {
		return dockerInspectData{}, fmt.Errorf("Expected to find exactly one image, but found %d", len(data))
	}

	return data[0], nil
}
