/*
 * Copyright 2016 ThoughtWorks, Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *  http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package agent

import (
	"errors"
	"fmt"
	"github.com/gocd-contrib/gocd-golang-agent/protocal"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"unicode"
)

type BuildSession struct {
	HttpClient *http.Client
	Send       chan *protocal.Message

	config      *Config
	buildStatus string
	console     *BuildConsole
	artifacts   *Uploader
	properties  *Uploader
	buildId     string
	envs        map[string]string
	cancel      chan bool
	done        chan bool
}

func MakeBuildSession(httpClient *http.Client, send chan *protocal.Message, config *Config) *BuildSession {
	return &BuildSession{
		HttpClient: httpClient,
		Send:       send,
		config:     config,
		cancel:     make(chan bool),
		done:       make(chan bool),
	}
}

func (s *BuildSession) Close() {
	close(s.cancel)
	<-s.done
}

func (s *BuildSession) isCanceled() bool {
	select {
	case <-s.cancel:
		return true
	default:
		return false
	}
}

func (s *BuildSession) Process(cmd *protocal.BuildCommand) error {
	defer func() {
		s.console.Close()
		close(s.done)
	}()
	err := s.process(cmd)
	if err != nil {
		s.fail(err)
	}
	return err
}

func (s *BuildSession) process(cmd *protocal.BuildCommand) error {
	if s.isCanceled() {
		LogDebug("Ignored command %v, because build is canceled", cmd.Name)
		return nil
	}

	LogDebug("procssing build command: %v\n", cmd)
	if s.buildStatus != "" && cmd.RunIfConfig != "any" && cmd.RunIfConfig != s.buildStatus {
		//skip, no failure
		return nil
	}
	if cmd.Test != nil {
		success := s.process(cmd.Test.Command) == nil
		if success != cmd.Test.Expectation {
			return nil
		}
	}

	switch cmd.Name {
	case "start":
		return s.processStart(cmd)
	case "compose":
		return s.processCompose(cmd)
	case "export":
		return s.processExport(cmd)
	case "test":
		return s.processTest(cmd)
	case "exec":
		return s.processExec(cmd)
	case "echo":
		return s.processEcho(cmd)
	case "uploadArtifact":
		return s.processUploadArtifact(cmd)
	case "reportCurrentStatus", "reportCompleting", "reportCompleted":
		jobState := cmd.Args["jobState"]
		s.Send <- protocal.ReportMessage(cmd.Name, s.statusReport(jobState))
	case "end":
		// do nothing
	default:
		s.console.WriteLn("TBI command: %v", cmd.Name)
	}
	return nil
}

func (s *BuildSession) processUploadArtifact(cmd *protocal.BuildCommand) (err error) {
	src := cmd.Args["src"]
	destDir := cmd.Args["dest"]

	wd, err := filepath.Abs(cmd.WorkingDirectory)
	if err != nil {
		return
	}
	absSrc := filepath.Join(wd, src)
	return s.uploadArtifacts(absSrc, destDir)
}

func (s *BuildSession) uploadArtifacts(source, destDir string) (err error) {
	if strings.Contains(source, "*") {
		matches, err := filepath.Glob(source)
		if err != nil {
			return err
		}
		base := BaseDirOfPathWithWildcard(source)
		baseLen := len(base)
		for _, file := range matches {
			fileDir, _ := filepath.Split(file)
			dest := Join("/", destDir, fileDir[baseLen:len(fileDir)-1])
			err = s.uploadArtifacts(file, dest)
			if err != nil {
				return err
			}
		}
		return nil
	}

	srcInfo, err := os.Stat(source)
	if err != nil {
		return
	}
	s.console.WriteLn("Uploading artifacts from %v to %v", source, destDescription(destDir))

	var destPath string
	if destDir != "" {
		destPath = Join("/", destDir, srcInfo.Name())
	} else {
		destPath = srcInfo.Name()
	}
	destURL, err := s.artifacts.BuildDestURL(destDir, s.buildId)
	if err != nil {
		return
	}
	return s.artifacts.Upload(source, destPath, destURL)
}

func (s *BuildSession) processExec(cmd *protocal.BuildCommand) error {
	arg0 := cmd.Args["command"]
	args := cmd.ExtractArgList(len(cmd.Args) - 1)
	execCmd := exec.Command(arg0, args...)
	execCmd.Stdout = s.console
	execCmd.Stderr = s.console
	execCmd.Dir = cmd.WorkingDirectory
	done := make(chan error)
	go func() {
		done <- execCmd.Run()
	}()

	select {
	case <-s.cancel:
		LogDebug("received cancel signal")
		LogInfo("killing process(%v) %v", execCmd.Process, cmd.Args)
		if err := execCmd.Process.Kill(); err != nil {
			s.console.WriteLn("kill command %v failed, error: %v", cmd.Args, err)
		} else {
			LogInfo("Process %v is killed", execCmd.Process)
		}
		return errors.New(fmt.Sprintf("%v is canceled", cmd.Args))
	case err := <-done:
		return err
	}
}

func (s *BuildSession) processTest(cmd *protocal.BuildCommand) error {
	flag := cmd.Args["flag"]
	targetPath := cmd.Args["path"]

	if "-d" == flag {
		_, err := os.Stat(targetPath)
		return err
	}
	return errors.New("unknown test flag")
}

func (s *BuildSession) statusReport(jobState string) map[string]interface{} {
	ret := map[string]interface{}{
		"agentRuntimeInfo": AgentRuntimeInfo(),
		"buildId":          s.buildId,
		"jobState":         jobState,
		"result":           capitalize(s.buildStatus)}
	return ret
}

func capitalize(str string) string {
	a := []rune(str)
	a[0] = unicode.ToUpper(a[0])
	return string(a)
}

func (s *BuildSession) processEcho(cmd *protocal.BuildCommand) error {
	for _, line := range cmd.ExtractArgList(len(cmd.Args)) {
		s.console.WriteLn(line)
	}
	return nil
}

func (s *BuildSession) processExport(cmd *protocal.BuildCommand) error {
	if len(cmd.Args) > 0 {
		for key, value := range cmd.Args {
			s.envs[key] = value
		}
	} else {
		exports := make([]string, len(s.envs))
		i := 0
		for key, value := range s.envs {
			exports[i] = fmt.Sprintf("export %v=%v", key, value)
			i++
		}
		sort.Strings(exports)
		for _, exp := range exports {
			s.console.WriteLn(exp)
		}
	}
	return nil
}

func (s *BuildSession) processCompose(cmd *protocal.BuildCommand) error {
	var err error
	for _, sub := range cmd.SubCommands {
		if err = s.process(sub); err != nil {
			s.fail(err)
		}
	}
	return err
}

func (s *BuildSession) fail(err error) {
	if s.buildStatus != "failed" {
		s.console.WriteLn(err.Error())
		s.buildStatus = "failed"
	}
}

func (s *BuildSession) processStart(cmd *protocal.BuildCommand) error {
	settings := cmd.Args

	SetState("buildLocator", settings["buildLocator"])
	SetState("buildLocatorForDisplay", settings["buildLocatorForDisplay"])

	curl, err := url.Parse(s.config.MakeFullServerURL(settings["consoleURI"]))
	if err != nil {
		return err
	}
	s.console = MakeBuildConsole(s.HttpClient, curl)
	s.artifacts = NewUploader(s.HttpClient, s.config.MakeFullServerURL(settings["artifactUploadBaseUrl"]))
	s.properties = NewUploader(s.HttpClient, s.config.MakeFullServerURL(settings["propertyBaseUrl"]))
	s.buildId = settings["buildId"]
	s.envs = make(map[string]string)
	s.buildStatus = "passed"
	return nil
}

func destDescription(path string) string {
	if path == "" {
		return "[defaultRoot]"
	} else {
		return path
	}
}
