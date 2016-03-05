package libgogoagent

import (
	"errors"
	"net/http"
	"os"
	"runtime"
	"time"
)

var buildSession *BuildSession

func registerData() map[string]string {
	hostname, _ := os.Hostname()
	workingDir, _ := os.Getwd()

	return map[string]string{
		"hostname":                      hostname,
		"uuid":                          ConfigGetAgentUUID(),
		"location":                      workingDir,
		"operatingSystem":               runtime.GOOS,
		"usablespace":                   "5000000000",
		"agentAutoRegisterKey":          agentAutoRegisterKey,
		"agentAutoRegisterResources":    agentAutoRegisterResources,
		"agentAutoRegisterEnvironments": agentAutoRegisterEnvironments,
		"agentAutoRegisterHostname":     hostname,
		"elasticAgentId":                agentAutoRegisterElasticAgentId,
		"elasticPluginId":               agentAutoRegisterElasticPluginId,
	}
}

func StartAgent() {
	send := make(chan Message)
	go ping(send)
	for {
		err := doStartAgent(send)
		if err != nil {
			LogInfo("something wrong: %v", err.Error())
		}
		LogInfo("sleep 10 seconds and restart")
		time.Sleep(10 * time.Second)
	}
}

func closeBuildSession() {
	if buildSession != nil {
		buildSession.Close()
		buildSession = nil
	}
}

func doStartAgent(send chan Message) error {
	err := Register(registerData())
	if err != nil {
		return err
	}

	httpClient, err := GoServerRemoteClient(true)
	if err != nil {
		return err
	}
	conn, err := MakeWebsocketConnection(ConfigGetWsServerURL(), ConfigGetHttpsServerURL("/"))
	if err != nil {
		return err
	}
	defer conn.Close()
	defer closeBuildSession()
	for {
		select {
		case msg := <-send:
			err := conn.Send(&msg)
			if err != nil {
				return err
			}
		case msg, ok := <-conn.Received:
			if !ok {
				return errors.New("Websocket connection closed")
			}
			err := processMessage(&msg, httpClient, send)
			if err != nil {
				return err
			}
		}
	}
}

func processMessage(msg *Message, httpClient *http.Client, send chan Message) error {
	switch msg.Action {
	case "setCookie":
		str, _ := msg.Data["data"].(string)
		SetState("cookie", str)
	case "cancelJob":
		closeBuildSession()
	case "reregister":
		CleanRegistration()
		return errors.New("received reregister message")
	case "cmd":
		closeBuildSession()
		buildSession = MakeBuildSession(httpClient, send)
		go processBuildCommandMessage(msg, buildSession)
	default:
		LogInfo("ERROR: unknown message action %v", msg)
	}
	return nil
}

func processBuildCommandMessage(msg *Message, buildSession *BuildSession) {
	SetState("runtimeStatus", "Building")
	command, _ := msg.Data["data"].(map[string]interface{})
	LogInfo("start process build command")
	err := buildSession.Process(MakeBuildCommand(command))
	SetState("runtimeStatus", "Idle")
	if err != nil {
		LogInfo("Error(%v) when processing message : %v", err, msg)
	}
}

func ping(send chan Message) {
	msgType := "com.thoughtworks.go.server.service.ElasticAgentRuntimeInfo"
	for {
		send <- Message{
			Action: "ping",
			Data: map[string]interface{}{
				"type": msgType,
				"data": AgentRuntimeInfo()}}
		time.Sleep(10 * time.Second)
	}
}
