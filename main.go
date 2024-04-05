package main

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	mpv "github.com/nbr23/go-mpv"
	"golang.org/x/net/html/charset"
)

/* XML PARSING */

type channel struct {
	Title      string   `xml:"title"`
	HighestURL string   `xml:"highestpls"`
	FastURL    []string `xml:"fastpls"`
	SlowURL    string   `xml:"slowpls"`
	Id         string   `xml:"id,attr"`
}

type channels struct {
	Channels []channel `xml:"channel"`
}

func getSomaChannels() (*channels, error) {
	res, err := http.Get("https://somafm.com/channels.xml")
	if err != nil {
		return nil, err
	}

	defer res.Body.Close()

	body, err := io.ReadAll(res.Body)
	if err != nil {
		return nil, err
	}

	var c channels

	reader := bytes.NewReader(body)
	decoder := xml.NewDecoder(reader)
	decoder.CharsetReader = charset.NewReaderLabel
	err = decoder.Decode(&c)
	if err != nil {
		return nil, err
	}

	return &c, nil
}

/* TUI */

type model struct {
	choices       []channel
	cursor        int
	playing       int
	mpvConfig     *mpvConfig
	quitting      bool
	config        *somaConfig
	currentlTitle string
}

type currentTitleUpdateMsg struct {
	title string
}

func initialModel(c []channel, m *mpvConfig) model {
	model := model{
		choices:   c,
		playing:   -1,
		mpvConfig: m,
		quitting:  false,
	}

	config, _ := loadConfig()
	model.config = config

	mpvCurrentlyPlayingPath, err := m.mpv.Path()
	if err != nil {
		panic(err)
	}
	if mpvCurrentlyPlayingPath != "" {
		for i, c := range c {
			if c.HighestURL == mpvCurrentlyPlayingPath {
				model.cursor = i
				model.playing = i
				model.mpvConfig.mpv.SetPause(model.config.IsPaused)
				break
			}
		}
	} else {
		if model.config.CurrentlyPlaying != "" {
			for i, c := range c {
				if c.HighestURL == model.config.CurrentlyPlaying {
					model.cursor = i
					if !model.config.IsPaused {
						model.playing = i
						model.mpvConfig.mpv.Loadfile(c.HighestURL, mpv.LoadFileModeReplace)
					}
					break
				}
			}
		}
	}

	return model
}

func (m model) Init() tea.Cmd {
	return nil
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case currentTitleUpdateMsg:
		m.currentlTitle = msg.title
	case tea.KeyMsg:
		switch msg.String() {

		case "ctrl+c", "q":
			m.config.saveConfig()
			if m.mpvConfig.signals != nil {
				m.mpvConfig.signals <- os.Kill
			} else {
				m.mpvConfig.mpv.SetPause(true)
			}
			m.quitting = true
			return m, tea.Quit

		case "up", "k":
			if m.cursor > 0 {
				m.cursor--
			}

		case "down", "j":
			if m.cursor < len(m.choices)-1 {
				m.cursor++
			}

		case "enter", " ":
			if m.playing != m.cursor {
				m.playing = m.cursor
				m.mpvConfig.mpv.Loadfile(m.choices[m.cursor].HighestURL, mpv.LoadFileModeReplace)
				m.config.CurrentlyPlaying = m.choices[m.cursor].HighestURL
				m.config.IsPaused = false
				if paused, _ := m.mpvConfig.mpv.Pause(); paused {
					m.mpvConfig.mpv.SetPause(false)
				}
			} else {
				m.mpvConfig.mpv.SetPause(true)
				m.config.IsPaused = true
				m.playing = -1
			}
		}
	}
	return m, nil
}

func (m model) View() string {
	if m.quitting {
		return ""
	}
	var s string
	if m.currentlTitle != "" {
		s = fmt.Sprintf("Now playing: « %s »\n\n", m.currentlTitle)
	} else {
		s = "Pick a SomaFM Channel:\n\n"
	}

	for i, choice := range m.choices {
		cursor := " "
		if m.cursor == i {
			cursor = ">"
		}

		checked := " "
		if m.playing == i {
			checked = "🔊"
		}

		s += fmt.Sprintf("%s %s %s\n", cursor, checked, choice.Title)
	}
	return s
}

/* MPV */

type mpvConfig struct {
	socketPath string
	startMpv   bool
	signals    chan os.Signal
	mpv        *mpv.Client
	ipccClient *mpv.IPCClient
}

func runMpv(c *mpvConfig) error {
	cmd := exec.Command("mpv", "--idle", fmt.Sprintf("--input-ipc-server=%s", c.socketPath))

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("error starting mpv: %s", err)
	}

	c.signals = make(chan os.Signal, 1)
	signal.Notify(c.signals, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-c.signals
		if err := cmd.Process.Kill(); err != nil {
			fmt.Printf("Error killing process: %s\n", err)
		}
		if err := cmd.Wait(); err != nil {
			fmt.Printf("Error waiting for command: %s\n", err)
		}
		os.Exit(1)
	}()

	return nil
}

func (m *mpvConfig) startMpvClient() error {
	ipcc, err := mpv.NewIPCClient(m.socketPath)
	if err != nil {
		if m.startMpv {
			err = runMpv(m)
			for i := 0; i < 15; i++ {
				ipcc, err = mpv.NewIPCClient(m.socketPath)
				if err == nil {
					break
				}
				time.Sleep(1 * time.Second)
			}
			if err != nil {
				return fmt.Errorf("error connecting to mpv: %s\n", err)
			}
		} else {
			return fmt.Errorf("error connecting to mpv: %s\n", err)
		}
	}
	m.ipccClient = ipcc
	m.mpv = mpv.NewClient(m.ipccClient)
	return nil
}

func (m *model) RegisterMpvEventHandler(p *tea.Program) {
	m.mpvConfig.mpv.ObserveProperty("media-title")
	m.mpvConfig.mpv.RegisterHandler(func(r *mpv.Response) {
		if r.Event == "property-change" && r.Name == "media-title" {
			p.Send(currentTitleUpdateMsg{title: r.Data.(string)})
		}
	})
}

/* CONFIG */

type somaConfig struct {
	CurrentlyPlaying string `json:"currentlyPlaying"`
	IsPaused         bool   `json:"isPaused"`
}

func (c *somaConfig) saveConfig() error {
	if c == nil {
		return nil
	}
	configDir, err := os.UserConfigDir()
	if err != nil {
		return err
	}

	configPath := filepath.Join(configDir, "soma.json")

	file, err := os.OpenFile(configPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
	if err != nil {
		return err
	}
	defer file.Close()

	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}

	_, err = file.Write(data)
	if err != nil {
		return err
	}

	return nil
}

func loadConfig() (*somaConfig, error) {
	configDir, err := os.UserConfigDir()
	if err != nil {
		return &somaConfig{}, err
	}

	configPath := filepath.Join(configDir, "soma.json")

	file, err := os.Open(configPath)
	if err != nil {
		return &somaConfig{}, err
	}
	defer file.Close()

	var c somaConfig

	decoder := json.NewDecoder(file)
	err = decoder.Decode(&c)
	if err != nil {
		return &somaConfig{}, err
	}

	return &c, nil
}

/* MAIN */

func main() {
	flags := flag.NewFlagSet("soma", flag.ExitOnError)
	socketPath := flags.String("socket", "/tmp/mpvsocket.sock", "Path to mpv socket")
	startMpv := flags.Bool("start-mpv", true, "Start mpv if not running")
	flags.Parse(os.Args[1:])

	s, err := getSomaChannels()
	if err != nil {
		fmt.Println("Unable to fetch Somafm stations", err)
		os.Exit(1)
	}

	mpvClient := mpvConfig{
		socketPath: *socketPath,
		startMpv:   *startMpv,
	}

	err = mpvClient.startMpvClient()
	if err != nil {
		fmt.Println("Unable to connect to mpv", err)
		os.Exit(1)
	}

	model := initialModel(s.Channels, &mpvClient)

	p := tea.NewProgram(model)

	model.RegisterMpvEventHandler(p)

	if _, err := p.Run(); err != nil {
		fmt.Print(err)
		os.Exit(1)
	}
}
