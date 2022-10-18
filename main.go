package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"regexp"
	"strconv"

	"strings"
	"sync"
	"time"

	docker "github.com/fsouza/go-dockerclient"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api"
)

const (
	defaultPumpName            = "pump"
	pumpEventStatusStartName   = "start"
	pumpEventStatusRestartName = "restart"
	pumpEventStatusRenameName  = "rename"
	pumpEventStatusDieName     = "die"
	trueString                 = "true"
	pumpMaxIDLen               = 12
)

var (
	Bot       *tgbotapi.BotAPI
	allowTTY  bool
	matchre   *regexp.Regexp
	sendTg    bool
	tgChatID  int64
	tgToken   string
	buildTime string
)

func setAllowTTY() {
	if t := GetEnvDefault("ALLOW_TTY", ""); t == trueString {
		allowTTY = true
	}
	debug("setting allowTTY to:", allowTTY)
}

type LogsPump struct {
	mu     sync.Mutex
	pumps  map[string]*containerPump
	client *docker.Client
}

func normalID(id string) string {
	if len(id) > pumpMaxIDLen {
		return id[:pumpMaxIDLen]
	}
	return id
}

func (p *LogsPump) Setup() error {
	var err error
	p.client, err = docker.NewClientFromEnv()
	return err
}

func debug(v ...interface{}) {
	if os.Getenv("DEBUG") != "" {
		log.Println(v...)
	}
}

func getInactivityTimeoutFromEnv() time.Duration {
	inactivityTimeout, err := time.ParseDuration(GetEnvDefault("INACTIVITY_TIMEOUT", "0"))
	assert(err, "Couldn't parse env var INACTIVITY_TIMEOUT. See https://golang.org/pkg/time/#ParseDuration for valid format.")
	return inactivityTimeout
}

func assert(err error, context string) {
	if err != nil {
		log.Fatal(context+": ", err)
	}
}

func (p *LogsPump) rename(event *docker.APIEvents) {
	p.mu.Lock()
	defer p.mu.Unlock()
	container, _ := p.client.InspectContainerWithOptions(docker.InspectContainerOptions{ID: event.ID})
	pump, ok := p.pumps[normalID(event.ID)]
	if !ok {
		debug("pump.rename(): ignore: pump not found, state:", container.State.StateString())
		return
	}
	pump.container.Name = container.Name
}

func backlog() bool {
	return os.Getenv("BACKLOG") == trueString
}

// Run executes the pump
func (p *LogsPump) Run() error {
	inactivityTimeout := getInactivityTimeoutFromEnv()
	debug("pump.Run(): using inactivity timeout: ", inactivityTimeout)

	containers, err := p.client.ListContainers(docker.ListContainersOptions{})
	if err != nil {
		return err
	}
	for idx := range containers {
		p.pumpLogs(&docker.APIEvents{
			ID:     normalID(containers[idx].ID),
			Status: pumpEventStatusStartName,
		}, backlog(), inactivityTimeout)
	}
	events := make(chan *docker.APIEvents)
	err = p.client.AddEventListener(events)
	if err != nil {
		return err
	}
	for event := range events {
		debug("pump.Run() event:", normalID(event.ID), event.Status)
		switch event.Status {
		case pumpEventStatusStartName, pumpEventStatusRestartName:
			go p.pumpLogs(event, backlog(), inactivityTimeout)
		case pumpEventStatusRenameName:
			go p.rename(event)
		case pumpEventStatusDieName:
			go p.reportDead(event)
		}
	}
	return errors.New("docker event stream closed")
}

func logDriverSupported(container *docker.Container) bool {
	switch container.HostConfig.LogConfig.Type {
	case "json-file", "journald", "db":
		return true
	default:
		return false
	}
}

func ignoreContainer(container *docker.Container) bool {
	if strings.Contains(container.Name, "teleglogger") {
		debug("ignoreContainer(): ignore: container name contains 'teleglogger'")
		return true
	}
	return false
}

func ignoreContainerTTY(container *docker.Container) bool {
	if container.Config.Tty && !allowTTY {
		return true
	}
	return false
}

func GetEnv(name string) string {
	if val := os.Getenv(name); val != "" {
		return val
	}
	panic(fmt.Sprintf("Environment variable %s is not set", name))
}

func GetEnvDefault(name, dfault string) string {
	if val := os.Getenv(name); val != "" {
		return val
	}
	return dfault
}

func (p *LogsPump) reportDead(event *docker.APIEvents) {
	log.Println("reportDead():", event.ID)
	id := normalID(event.ID)
	c, err := p.client.InspectContainerWithOptions(docker.InspectContainerOptions{ID: id})
	if err != nil {
		log.Println("reportDead(): error inspecting container:", err)
	}

	msg := fmt.Sprintf("%s %s died", c.Name, c.ID[:4])
	tgString(msg)
}

func (p *LogsPump) pumpLogs(event *docker.APIEvents, backlog bool, inactivityTimeout time.Duration) { //nolint:gocyclo
	id := normalID(event.ID)
	log.Println("pump.pumpLogs(): start:", id)
	container, err := p.client.InspectContainerWithOptions(docker.InspectContainerOptions{ID: id})
	assert(err, defaultPumpName)
	if ignoreContainerTTY(container) {
		debug("pump.pumpLogs():", id, "ignored: tty enabled")
		return
	}
	if ignoreContainer(container) {
		debug("pump.pumpLogs():", id, "ignored: environ ignore")
		return
	}
	if !logDriverSupported(container) {
		debug("pump.pumpLogs():", id, "ignored: log driver not supported")
		return
	}

	var tail = GetEnvDefault("TAIL", "all")
	var sinceTime time.Time
	if backlog {
		sinceTime = time.Unix(0, 0)
	} else {
		sinceTime = time.Now()
	}

	p.mu.Lock()
	if _, exists := p.pumps[id]; exists {
		p.mu.Unlock()
		debug("pump.pumpLogs():", id, "pump exists")
		return
	}

	// RawTerminal with container Tty=false injects binary headers into
	// the log stream that show up as garbage unicode characters
	rawTerminal := false
	if allowTTY && container.Config.Tty {
		rawTerminal = true
	}
	outrd, outwr := io.Pipe()
	errrd, errwr := io.Pipe()
	p.pumps[id] = newContainerPump(container, outrd, errrd)
	p.mu.Unlock()
	go func() {
		for {
			debug("pump.pumpLogs():", id, "started, tail:", tail)
			err := p.client.Logs(docker.LogsOptions{
				Container:         id,
				OutputStream:      outwr,
				ErrorStream:       errwr,
				Stdout:            true,
				Stderr:            true,
				Follow:            true,
				Tail:              tail,
				Since:             sinceTime.Unix(),
				InactivityTimeout: inactivityTimeout,
				RawTerminal:       rawTerminal,
			})
			if err != nil {
				debug("pump.pumpLogs():", id, "stopped with error:", err)
			} else {
				debug("pump.pumpLogs():", id, "stopped")
			}

			sinceTime = time.Now()
			if err == docker.ErrInactivityTimeout {
				sinceTime = sinceTime.Add(-inactivityTimeout)
			}

			container, err := p.client.InspectContainerWithOptions(docker.InspectContainerOptions{ID: id})
			if err != nil {
				_, four04 := err.(*docker.NoSuchContainer)
				if !four04 {
					assert(err, defaultPumpName)
				}
			} else if container.State.Running {
				continue
			}

			debug("pump.pumpLogs():", id, "dead")
			outwr.Close()
			errwr.Close()
			p.mu.Lock()
			delete(p.pumps, id)
			p.mu.Unlock()
			return
		}
	}()
}

type Message struct {
	Container *docker.Container
	Source    string
	Data      string
	Time      time.Time
}

type containerPump struct {
	container *docker.Container
}

func newContainerPump(container *docker.Container, stdout, stderr io.Reader) *containerPump {
	cp := &containerPump{
		container: container,
	}
	pump := func(source string, input io.Reader) {
		buf := bufio.NewReader(input)
		for {
			line, err := buf.ReadString('\n')
			if err != nil {
				if err != io.EOF {
					debug("pump.newContainerPump():", normalID(container.ID), source+":", err)
				}
				return
			}
			cp.send(&Message{
				Data:      strings.TrimSuffix(line, "\n"),
				Container: container,
				Time:      time.Now(),
				Source:    source,
			})
		}
	}
	go pump("stdout", stdout)
	go pump("stderr", stderr)
	return cp
}

func (cp *containerPump) send(msg *Message) {
	if matchre.Match([]byte(msg.Data)) {
		debug("YES msg match pump.send():", normalID(cp.container.ID[:4]), msg.Source+":", msg.Data)
		err := tgMessage(msg)
		if err != nil {
			log.Println("TG send failed", err.Error())
		}
	} else {
		debug("NOT msg match pump.send():", normalID(cp.container.ID[:4]), msg.Source+":", msg.Data)
	}
}

func main() {
	log.Println("built", buildTime)
	etg := GetEnv("TG")
	if (etg != "") && (etg != "0") {
		sendTg = true

		tgChatIDString := GetEnv("TG_CHAT")
		var err error
		tgChatID, err = strconv.ParseInt(tgChatIDString, 10, 64)
		if err != nil {
			log.Fatal(err)
		}
		tgToken = GetEnv("TG_TOKEN")
		botInit()
	}

	var err error
	matchre, err = regexp.Compile(GetEnvDefault("MATCHRE", `(?i).*error.*`))
	if err != nil {
		log.Fatal(err)
	}

	setAllowTTY()
	p := &LogsPump{
		pumps: make(map[string]*containerPump),
	}
	err = p.Setup()
	if err != nil {
		log.Fatal(err)
	}
	err = p.Run()
	if err != nil {
		log.Fatal(err)
	}
}

func botInit() error {
	var err error
	Bot, err = tgbotapi.NewBotAPI(tgToken)
	if err != nil {
		return err
	}
	return nil
}

func tgString(msg string) error {
	if !sendTg {
		debug("NOT sending tgString():", msg)
		return nil
	}
	log.Println("Sending tgString():", msg)
	if Bot != nil {
		ts := time.Now().Format("Mon 02-Jan 15:04")
		hea := fmt.Sprintf("%s, %s", ts, msg)
		nm := tgbotapi.NewMessage(tgChatID, hea)
		_, err := Bot.Send(nm)
		return err
	}
	return nil
}

func tgMessage(m *Message) error {
	msg := fmt.Sprintf("%s %s %s %s", m.Container.Name, m.Container.ID[:4], m.Source, m.Data)
	if !sendTg {
		debug("NOT sending tgMessage():", msg)
		return nil
	}
	log.Println("Sending tgMessage():", msg)
	if Bot != nil {
		ts := time.Now().Format("Mon 02-Jan 15:04")
		hea := fmt.Sprintf("%s, %s", ts, msg)
		nm := tgbotapi.NewMessage(tgChatID, hea)
		_, err := Bot.Send(nm)
		return err
	}
	return nil
}
