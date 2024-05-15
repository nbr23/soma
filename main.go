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

	"github.com/charmbracelet/bubbles/list"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	mpv "github.com/nbr23/go-mpv"
	"golang.org/x/net/html/charset"
)

/* XML PARSING */

type channel struct {
	ChannelTitle       string   `xml:"title" json:"title"`
	HighestURL         string   `xml:"highestpls" json:"highestpls"`
	FastURL            []string `xml:"fastpls" json:"fastpls"`
	SlowURL            string   `xml:"slowpls" json:"slowpls"`
	Id                 string   `xml:"id,attr" json:"id"`
	ChannelDescription string   `xml:"description" json:"description"`
	Genre              string   `xml:"genre" json:"genre"`
	IsPlaying          *bool
}

func (c channel) FilterValue() string {
	return fmt.Sprintf("%s %s", c.Id, c.ChannelDescription)
}
func (c channel) Title() string {
	if *c.IsPlaying {
		return fmt.Sprintf("♫ %s", c.ChannelTitle)
	}
	return c.ChannelTitle
}
func (c channel) Description() string { return fmt.Sprintf("%s | %s", c.Genre, c.ChannelDescription) }

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
	docStyle           = lipgloss.NewStyle().Margin(1, 1)
	statusMessageStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("#00FF00")).
				Render
	titleStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#FFFDF5")).
			Bold(true)

	paginationActiveStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("#00AA00")).
				Bold(true)
	paginationInactiveStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("#909090"))

	cursorStyle = lipgloss.NewStyle().
			Bold(true).
			Padding(0, 0, 0, 1).
			Foreground(lipgloss.Color("#00FF00"))

	playingStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("#00AA00"))
)

type model struct {
	playing   string
	mpvConfig *mpvConfig
	quitting  bool
	config    *somaConfig
	list      list.Model
}

type currentTitleUpdateMsg struct {
	title string
}

type changePausedStatusMsg struct {
	paused bool
}

func newItemDelegate() list.DefaultDelegate {
	d := list.NewDefaultDelegate()
	d.Styles.SelectedTitle = cursorStyle
	d.Styles.SelectedDesc = cursorStyle
	d.SetSpacing(0)

	return d
}

func channelsToItems(c []channel) []list.Item {
	items := make([]list.Item, len(c))
	for i, ch := range c {
		ch.IsPlaying = new(bool)
		items[i] = ch
	}
	return items
}

func setIsPlaying(l list.Model, id string, isPlaying bool) {
	for _, c := range l.Items() {
		if c.(channel).Id == id {
			*c.(channel).IsPlaying = isPlaying
		} else {
			*c.(channel).IsPlaying = false
		}
	}
}

func initialModel(m *mpvConfig) model {
	model := model{
		playing:   "",
		mpvConfig: m,
		quitting:  false,
	}

	config, _ := loadConfig()
	model.config = config

	if len(model.config.Channels.Channels) == 0 || time.Since(model.config.LastChannelsListUpdate) > 24*time.Hour*7 {
		model.config.LastChannelsListUpdate = time.Now()
		c, err := getSomaChannels()
		if err != nil {
			fmt.Println("Unable to fetch Somafm stations", err)
			os.Exit(1)
		}
		model.config.Channels = *c
	}

	model.list = list.New(channelsToItems(model.config.Channels.Channels), newItemDelegate(), 0, 0)
	model.list.Title = "SomaFM"

	mpvCurrentlyPlayingPath, err := m.mpv.Path()
	if err != nil {
		panic(err)
	}
	if mpvCurrentlyPlayingPath != "" {
		for i, c := range model.config.Channels.Channels {
			if c.HighestURL == mpvCurrentlyPlayingPath {
				model.playing = c.Id
				model.mpvConfig.mpv.SetPause(model.config.IsPaused)
				model.list.Select(i)
				setIsPlaying(model.list, c.Id, model.config.IsPaused)
				break
			}
		}
		if model.playing == "" {
			model.mpvConfig.mpv.SetPause(true)
		}
	} else {
		if model.config.CurrentlyPlaying != "" {
			for i, c := range model.config.Channels.Channels {
				if c.Id == model.config.CurrentlyPlaying {
					model.list.Select(i)
					if !model.config.IsPaused {
						model.playing = c.Id
						model.mpvConfig.mpv.Loadfile(c.HighestURL, mpv.LoadFileModeReplace)
						setIsPlaying(model.list, c.Id, true)
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
	m.playing = m.list.SelectedItem().(channel).Id
	m.mpvConfig.mpv.Loadfile(m.list.SelectedItem().(channel).HighestURL, mpv.LoadFileModeReplace)
	m.config.CurrentlyPlaying = m.list.SelectedItem().(channel).Id
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		top, right, bottom, left := docStyle.GetMargin()
		m.list.SetSize(msg.Width-left-right, msg.Height-top-bottom)
	case currentTitleUpdateMsg:
		m.list.NewStatusMessage(statusMessageStyle(fmt.Sprintf("♫ Now playing: « %s | %s »", m.list.SelectedItem().(channel).ChannelTitle, msg.title)))
	case changePausedStatusMsg:
		if msg.paused {
			setIsPlaying(m.list, m.playing, false)
			m.config.IsPaused = true
			m.playing = ""
			m.list.NewStatusMessage("")
		} else {
			m.config.IsPaused = false
			m.playing = m.config.CurrentlyPlaying
			setIsPlaying(m.list, m.playing, true)
			title, _ := m.mpvConfig.mpv.GetProperty("media-title")
			m.list.NewStatusMessage(statusMessageStyle(fmt.Sprintf("♫ Now playing: « %s | %s »", m.config.CurrentlyPlaying, title)))

		}
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

		case "enter":
			if m.playing != m.list.SelectedItem().(channel).Id {
				m.PlaySelectedChannel()
				setIsPlaying(m.list, m.list.SelectedItem().(channel).Id, true)
				m.config.IsPaused = false
				m.playing = m.list.SelectedItem().(channel).Id
				if paused, _ := m.mpvConfig.mpv.Pause(); paused {
					m.mpvConfig.mpv.SetPause(false)
				}
			} else {
				setIsPlaying(m.list, m.playing, false)
				m.mpvConfig.mpv.SetPause(true)
				m.config.IsPaused = true
				m.playing = ""
				m.list.NewStatusMessage("")
			}
		}
	}
	var cmd tea.Cmd
	m.list, cmd = m.list.Update(msg)
	return m, cmd
}

func (m model) View() string {
	if m.quitting {
		return ""
	}
	return docStyle.Render(m.list.View())
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
				return fmt.Errorf("error connecting to mpv: %s", err)
			}
		} else {
			return fmt.Errorf("error connecting to mpv: %s", err)
		}
	}
	m.ipccClient = ipcc
	m.mpv = mpv.NewClient(m.ipccClient)
	return nil
}

func (m *model) RegisterMpvEventHandler(p *tea.Program) {
	m.mpvConfig.mpv.ObserveProperty("media-title")
	m.mpvConfig.mpv.ObserveProperty("core-idle")
	m.mpvConfig.mpv.RegisterHandler(func(r *mpv.Response) {
		if r.Event == "property-change" && r.Name == "media-title" {
			if r.Data == nil {
				return
			}
			p.Send(currentTitleUpdateMsg{title: r.Data.(string)})
		} else if r.Event == "property-change" && r.Name == "core-idle" {
			if r.Data == nil {
				return
			}
			p.Send(changePausedStatusMsg{paused: r.Data.(bool)})
		}
	})
}

/* CONFIG */

type somaConfig struct {
	CurrentlyPlaying       string    `json:"currentlyPlaying"`
	IsPaused               bool      `json:"isPaused"`
	Channels               channels  `json:"channels"`
	LastChannelsListUpdate time.Time `json:"lastChannelsListUpdate"`
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
	model.list.SetShowPagination(true)
	model.list.SetShowStatusBar(false)
	model.list.Styles.Title = titleStyle

	model.list.Paginator.ActiveDot = paginationActiveStyle.Render("•")
	model.list.Paginator.InactiveDot = paginationInactiveStyle.Render("•")

	p := tea.NewProgram(model)

	model.RegisterMpvEventHandler(p)

	if _, err := p.Run(); err != nil {
		fmt.Print(err)
		os.Exit(1)
	}
}
