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
	"github.com/charmbracelet/lipgloss"

	mpv "github.com/nbr23/go-mpv"
	"golang.org/x/net/html/charset"
)

/* XML PARSING */

type channel struct {
	Title       string   `xml:"title" json:"title"`
	HighestURL  string   `xml:"highestpls" json:"highestpls"`
	FastURL     []string `xml:"fastpls" json:"fastpls"`
	SlowURL     string   `xml:"slowpls" json:"slowpls"`
	Id          string   `xml:"id,attr" json:"id"`
	Description string   `xml:"description" json:"description"`
	Genre       string   `xml:"genre" json:"genre"`
}

type channels struct {
	Channels []channel `xml:"channel" json:"channels"`
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

var (
	appStyle        = lipgloss.NewStyle()
	nowPlayingStyle = lipgloss.NewStyle().
			Inherit(appStyle).
			Bold(true).
			Border(lipgloss.DoubleBorder()).
			Render
	textStyle = lipgloss.NewStyle().
			Inherit(appStyle).
			Render
	currentChannelStyle = lipgloss.NewStyle().
				Inherit(appStyle).
				Bold(true).
				Render
	cursorStyle = lipgloss.NewStyle().
			Inherit(appStyle).
			Bold(true).
			Foreground(lipgloss.Color("#00FF00")).
			Render
)

type model struct {
	cursor        int
	playing       int
	mpvConfig     *mpvConfig
	quitting      bool
	config        *somaConfig
	currentlTitle string
	width         int
	height        int
}

type currentTitleUpdateMsg struct {
	title string
}

func initialModel(m *mpvConfig) model {
	model := model{
		playing:   -1,
		mpvConfig: m,
		quitting:  false,
	}

	config, _ := loadConfig()
	model.config = config

	if len(model.config.Channels.Channels) == 0 {
		c, err := getSomaChannels()
		if err != nil {
			fmt.Println("Unable to fetch Somafm stations", err)
			os.Exit(1)
		}
		model.config.Channels = *c
	}

	mpvCurrentlyPlayingPath, err := m.mpv.Path()
	if err != nil {
		panic(err)
	}
	if mpvCurrentlyPlayingPath != "" {
		for i, c := range model.config.Channels.Channels {
			if c.HighestURL == mpvCurrentlyPlayingPath {
				model.cursor = i
				model.playing = i
				model.mpvConfig.mpv.SetPause(model.config.IsPaused)
				break
			}
		}
	} else {
		if model.config.CurrentlyPlaying != "" {
			for i, c := range model.config.Channels.Channels {
				if c.Id == model.config.CurrentlyPlaying {
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

func (m *model) PlaySelectedChannel() {
	m.playing = m.cursor
	m.mpvConfig.mpv.Loadfile(m.config.Channels.Channels[m.cursor].HighestURL, mpv.LoadFileModeReplace)
	m.config.CurrentlyPlaying = m.config.Channels.Channels[m.cursor].Id
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil
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
			} else {
				m.cursor = len(m.config.Channels.Channels) - 1
			}

		case "down", "j":
			if m.cursor < len(m.config.Channels.Channels)-1 {
				m.cursor++
			} else {
				m.cursor = 0
			}

		case "left", "h":
			if m.cursor > 0 {
				m.cursor--
			} else {
				m.cursor = len(m.config.Channels.Channels) - 1
			}
			m.PlaySelectedChannel()

		case "right", "l":
			if m.cursor < len(m.config.Channels.Channels)-1 {
				m.cursor++
			} else {
				m.cursor = 0
			}
			m.PlaySelectedChannel()

		case "enter", " ":
			if m.playing != m.cursor {
				m.PlaySelectedChannel()
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
	var s []string
	if m.currentlTitle != "" && m.playing != -1 {
		s = append(s, nowPlayingStyle(fmt.Sprintf("Now playing: « %s »", m.currentlTitle)))
	} else {
		s = append(s, nowPlayingStyle("Pick a SomaFM Channel"))
	}

	for i, choice := range m.config.Channels.Channels {
		cursor := " "
		choiceTitle := fmt.Sprintf("%s", choice.Title)
		if m.cursor == i {
			cursor = ">"
			choiceTitle = fmt.Sprintf("%s | %s | %s", choice.Title, choice.Genre, choice.Description)
		}

		checked := " "
		if m.playing == i {
			checked = "♫"
			s = append(s, lipgloss.JoinHorizontal(0, cursorStyle(cursor), currentChannelStyle(fmt.Sprintf(" %s %s", checked, choiceTitle))))
		} else {
			s = append(s, lipgloss.JoinHorizontal(0, cursorStyle(cursor), textStyle(fmt.Sprintf(" %s %s", checked, choiceTitle))))
		}

	}
	return appStyle.Copy().
		Width(m.width).
		Height(m.height).
		Render(lipgloss.JoinVertical(0, s...))
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
			if r.Data == nil {
				return
			}
			p.Send(currentTitleUpdateMsg{title: r.Data.(string)})
		}
	})
}

/* CONFIG */

type somaConfig struct {
	CurrentlyPlaying string   `json:"currentlyPlaying"`
	IsPaused         bool     `json:"isPaused"`
	Channels         channels `json:"channels"`
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

	mpvClient := mpvConfig{
		socketPath: *socketPath,
		startMpv:   *startMpv,
	}

	err := mpvClient.startMpvClient()
	if err != nil {
		fmt.Println("Unable to connect to mpv", err)
		os.Exit(1)
	}

	model := initialModel(&mpvClient)

	p := tea.NewProgram(model)

	model.RegisterMpvEventHandler(p)

	if _, err := p.Run(); err != nil {
		fmt.Print(err)
		os.Exit(1)
	}
}
