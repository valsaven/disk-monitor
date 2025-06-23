package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
	"unsafe"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/guptarohit/asciigraph"
)

// DiskInfo holds disk information
type DiskInfo struct {
	Drive      string `json:"drive"`
	TotalSpace uint64 `json:"total_space"`
	FreeSpace  uint64 `json:"free_space"`
	UsedSpace  uint64 `json:"used_space"`
}

// Snapshot represents a snapshot of all disks at a point in time
type Snapshot struct {
	Timestamp time.Time  `json:"timestamp"`
	Disks     []DiskInfo `json:"disks"`
}

// HistoryData holds the full history of snapshots
type HistoryData struct {
	Snapshots []Snapshot `json:"snapshots"`
}

var (
	kernel32            = syscall.NewLazyDLL("kernel32.dll")
	getDiskFreeSpaceExW = kernel32.NewProc("GetDiskFreeSpaceExW")
	getLogicalDrives    = kernel32.NewProc("GetLogicalDrives")
)

// UI styles
var (
	titleStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("86")).
			MarginBottom(1)

	headerStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("170"))

	diskNameStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("86"))

	helpStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("241"))

	selectedStyle = lipgloss.NewStyle().
			Background(lipgloss.Color("237")).
			Bold(true)

	// Colors for graph lines
	lineColors = []lipgloss.Color{
		lipgloss.Color("9"),   // Red
		lipgloss.Color("10"),  // Green
		lipgloss.Color("11"),  // Yellow
		lipgloss.Color("12"),  // Blue
		lipgloss.Color("13"),  // Magenta
		lipgloss.Color("14"),  // Cyan
		lipgloss.Color("202"), // Orange
		lipgloss.Color("199"), // Pink
	}
)

// getDiskSpace retrieves space info for a drive
func getDiskSpace(drive string) (*DiskInfo, error) {
	var freeBytesAvailable, totalNumberOfBytes, totalNumberOfFreeBytes uint64

	drivePath, err := syscall.UTF16PtrFromString(drive)
	if err != nil {
		return nil, fmt.Errorf("failed to convert path: %v", err)
	}

	// Set up timeout for the operation
	done := make(chan bool)
	var result *DiskInfo
	var resultErr error

	go func() {
		ret, _, err := getDiskFreeSpaceExW.Call(
			uintptr(unsafe.Pointer(drivePath)),
			uintptr(unsafe.Pointer(&freeBytesAvailable)),
			uintptr(unsafe.Pointer(&totalNumberOfBytes)),
			uintptr(unsafe.Pointer(&totalNumberOfFreeBytes)),
		)

		if ret == 0 {
			resultErr = fmt.Errorf("failed to get disk info for %s: %v", drive, err)
		} else {
			result = &DiskInfo{
				Drive:      drive,
				TotalSpace: totalNumberOfBytes,
				FreeSpace:  freeBytesAvailable,
				UsedSpace:  totalNumberOfBytes - freeBytesAvailable,
			}
		}
		done <- true
	}()

	// Wait with timeout
	select {
	case <-done:
		return result, resultErr
	case <-time.After(2 * time.Second):
		return nil, fmt.Errorf("timeout getting disk info for %s", drive)
	}
}

// getAvailableDrives returns list of available local drives
func getAvailableDrives() []string {
	drives := []string{}
	ret, _, _ := getLogicalDrives.Call()

	driveBits := uint32(ret)
	for i := 0; i < 26; i++ {
		if driveBits&(1<<uint(i)) != 0 {
			drive := fmt.Sprintf("%c:\\", 'A'+i)
			// Check drive type
			driveType := getDriveType(drive)
			// Skip CD-ROM and network drives
			if driveType != DRIVE_CDROM && driveType != DRIVE_REMOTE {
				drives = append(drives, drive)
			}
		}
	}

	return drives
}

// Drive type constants
const (
	DRIVE_UNKNOWN     = 0
	DRIVE_NO_ROOT_DIR = 1
	DRIVE_REMOVABLE   = 2
	DRIVE_FIXED       = 3
	DRIVE_REMOTE      = 4
	DRIVE_CDROM       = 5
	DRIVE_RAMDISK     = 6
)

// getDriveType returns the type of the drive
func getDriveType(drive string) uint32 {
	kernel32 := syscall.NewLazyDLL("kernel32.dll")
	getDriveTypeW := kernel32.NewProc("GetDriveTypeW")

	drivePath, _ := syscall.UTF16PtrFromString(drive)
	ret, _, _ := getDriveTypeW.Call(uintptr(unsafe.Pointer(drivePath)))

	return uint32(ret)
}

// getAllDisksInfo gathers info for all drives
func getAllDisksInfo() []DiskInfo {
	var disks []DiskInfo
	drives := getAvailableDrives()

	// Channels for results
	results := make(chan *DiskInfo, len(drives))
	errors := make(chan error, len(drives))

	// Parallel collection
	for _, drive := range drives {
		go func(d string) {
			info, err := getDiskSpace(d)
			if err != nil {
				errors <- err
				results <- nil
			} else {
				results <- info
				errors <- nil
			}
		}(drive)
	}

	// Collect results
	for range drives {
		info := <-results
		err := <-errors
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			continue
		}
		if info != nil {
			disks = append(disks, *info)
		}
	}

	return disks
}

// getHistoryFilePath returns path to history file
func getHistoryFilePath() string {
	homeDir, _ := os.UserHomeDir()
	return filepath.Join(homeDir, "disk_monitor_history.json")
}

// loadHistory loads history from file
func loadHistory() (*HistoryData, error) {
	filePath := getHistoryFilePath()
	data, err := os.ReadFile(filePath)
	if err != nil {
		if os.IsNotExist(err) {
			return &HistoryData{Snapshots: []Snapshot{}}, nil
		}
		return nil, err
	}

	var history HistoryData
	if err := json.Unmarshal(data, &history); err != nil {
		return nil, err
	}

	return &history, nil
}

// saveHistory saves history to file
func saveHistory(history *HistoryData) error {
	filePath := getHistoryFilePath()
	data, err := json.MarshalIndent(history, "", "  ")
	if err != nil {
		return err
	}

	return os.WriteFile(filePath, data, 0644)
}

// formatBytes formats bytes into human-readable string
func formatBytes(bytes uint64) string {
	const unit = 1024
	if bytes < unit {
		return fmt.Sprintf("%d B", bytes)
	}
	div, exp := uint64(unit), 0
	for n := bytes / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(bytes)/float64(div), "KMGTPE"[exp])
}

// Model - Bubble Tea application model
type Model struct {
	history      *HistoryData
	graphs       map[string][]float64
	currentView  string
	selectedDisk int
	width        int
	height       int
	err          error
	loading      bool
	status       string
}

// viewType - display mode
type viewType string

const (
	viewChart   viewType = "chart"
	viewCurrent viewType = "current"
)

// NewModel creates a new model
func NewModel() Model {
	history, _ := loadHistory()

	return Model{
		history:     history,
		graphs:      make(map[string][]float64),
		currentView: string(viewCurrent),
		loading:     true,
		status:      "Loading data...",
	}
}

// Init initializes the model
func (m Model) Init() tea.Cmd {
	return tea.Batch(
		tea.WindowSize(),
		collectDataCmd,
	)
}

// collectDataCmd command to collect data
func collectDataCmd() tea.Msg {
	disks := getAllDisksInfo()
	return diskInfoMsg{disks: disks}
}

// diskInfoMsg message containing disk info
type diskInfoMsg struct {
	disks []DiskInfo
}

// Update handles messages
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c", "q", "esc":
			return m, tea.Quit
		case "tab":
			if m.loading {
				return m, nil
			}
			// Toggle view
			if m.currentView == string(viewChart) {
				m.currentView = string(viewCurrent)
			} else {
				m.currentView = string(viewChart)
			}
			m.updateChart()
		case "up", "k":
			if m.loading {
				return m, nil
			}
			if m.selectedDisk > 0 {
				m.selectedDisk--
				m.updateChart()
			}
		case "down", "j":
			if m.loading {
				return m, nil
			}
			drives := getAvailableDrives()
			if m.selectedDisk < len(drives)-1 {
				m.selectedDisk++
				m.updateChart()
			}
		case "r":
			if m.loading {
				return m, nil
			}
			// Refresh data
			m.loading = true
			m.status = "Refreshing data..."
			return m, collectDataCmd
		}
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		if !m.loading {
			m.updateChart()
		}
	case diskInfoMsg:
		if len(msg.disks) == 0 {
			m.err = fmt.Errorf("no drives found")
			m.loading = false
			return m, nil
		}

		snapshot := Snapshot{
			Timestamp: time.Now(),
			Disks:     msg.disks,
		}

		m.history.Snapshots = append(m.history.Snapshots, snapshot)
		if err := saveHistory(m.history); err != nil {
			m.err = err
		}

		m.loading = false
		m.status = ""
		m.updateChart()
	}

	return m, nil
}

// collectData collects new data
func (m *Model) collectData() {
	disks := getAllDisksInfo()
	if len(disks) == 0 {
		m.err = fmt.Errorf("no drives found")
		return
	}

	snapshot := Snapshot{
		Timestamp: time.Now(),
		Disks:     disks,
	}

	m.history.Snapshots = append(m.history.Snapshots, snapshot)
	if err := saveHistory(m.history); err != nil {
		m.err = err
	}
}

// updateChart updates graph data
func (m *Model) updateChart() {
	if len(m.history.Snapshots) < 2 {
		return
	}

	// Collect unique drives
	driveMap := make(map[string]bool)
	for _, snapshot := range m.history.Snapshots {
		for _, disk := range snapshot.Disks {
			driveMap[disk.Drive] = true
		}
	}

	// Sort drives for consistent order
	var drives []string
	for drive := range driveMap {
		drives = append(drives, drive)
	}
	sort.Strings(drives)

	// Gather data per drive
	m.graphs = make(map[string][]float64)
	for _, drive := range drives {
		var data []float64
		for _, snapshot := range m.history.Snapshots {
			for _, disk := range snapshot.Disks {
				if disk.Drive == drive {
					spaceGB := float64(disk.FreeSpace) / 1024 / 1024 / 1024
					data = append(data, spaceGB)
					break
				}
			}
		}
		m.graphs[drive] = data
	}
}

// View renders the UI
func (m Model) View() string {
	var s strings.Builder

	// Title
	s.WriteString(titleStyle.Render("Disk Space Monitor"))
	s.WriteString("\n\n")

	if m.loading {
		s.WriteString(lipgloss.NewStyle().Foreground(lipgloss.Color("86")).Render(
			fmt.Sprintf("%s\n", m.status)))
		return s.String()
	}

	if m.err != nil {
		s.WriteString(lipgloss.NewStyle().Foreground(lipgloss.Color("9")).Render(
			fmt.Sprintf("Error: %v\n", m.err)))
	}

	switch m.currentView {
	case string(viewCurrent):
		s.WriteString(m.renderCurrentView())
	case string(viewChart):
		s.WriteString(m.renderChartView())
	}

	// Help
	s.WriteString("\n\n")
	s.WriteString(helpStyle.Render(
		"tab: switch view • r: refresh • ↑↓: select drive • q: quit"))

	return s.String()
}

// renderCurrentView shows current disk state
func (m Model) renderCurrentView() string {
	var s strings.Builder

	s.WriteString(headerStyle.Render("Current disk status:"))
	s.WriteString("\n\n")

	disks := getAllDisksInfo()
	if len(disks) == 0 {
		s.WriteString("No drives found\n")
		return s.String()
	}

	for i, disk := range disks {
		diskLine := fmt.Sprintf("%s  Total: %s  Free: %s  Used: %s (%.1f%%)",
			diskNameStyle.Render(disk.Drive),
			formatBytes(disk.TotalSpace),
			formatBytes(disk.FreeSpace),
			formatBytes(disk.UsedSpace),
			float64(disk.UsedSpace)/float64(disk.TotalSpace)*100)

		if i == m.selectedDisk {
			s.WriteString(selectedStyle.Render(diskLine))
		} else {
			s.WriteString(diskLine)
		}
		s.WriteString("\n")

		// Progress bar
		barWidth := 50
		usedPercent := float64(disk.UsedSpace) / float64(disk.TotalSpace)
		filledWidth := int(usedPercent * float64(barWidth))

		bar := strings.Repeat("█", filledWidth) + strings.Repeat("░", barWidth-filledWidth)
		barColor := lipgloss.Color("10") // Green
		if usedPercent > 0.8 {
			barColor = lipgloss.Color("9") // Red
		} else if usedPercent > 0.6 {
			barColor = lipgloss.Color("11") // Yellow
		}

		s.WriteString("  ")
		s.WriteString(lipgloss.NewStyle().Foreground(barColor).Render(bar))
		s.WriteString("\n\n")
	}

	// Last update info
	if len(m.history.Snapshots) > 0 {
		lastSnapshot := m.history.Snapshots[len(m.history.Snapshots)-1]
		s.WriteString(helpStyle.Render(fmt.Sprintf(
			"Last update: %s",
			lastSnapshot.Timestamp.Format("2006-01-02 15:04:05"))))
	}

	return s.String()
}

// renderChartView displays the graph
func (m Model) renderChartView() string {
	var s strings.Builder

	s.WriteString(headerStyle.Render("Free space over time:"))
	s.WriteString("\n\n")

	if len(m.history.Snapshots) < 2 {
		s.WriteString("Not enough data for a graph yet.\n")
		s.WriteString("Run the program a few times to build history.\n")
		return s.String()
	}

	// Graph height
	height := m.height - 20
	if height < 10 {
		height = 10
	}

	// Get data for selected drive
	drives := getAvailableDrives()
	if m.selectedDisk >= 0 && m.selectedDisk < len(drives) {
		selectedDrive := drives[m.selectedDisk]
		var dataPoints []float64
		var timeLabels []string
		var lastTime time.Time

		// Collect points and time labels
		for i, snapshot := range m.history.Snapshots {
			for _, disk := range snapshot.Disks {
				if disk.Drive == selectedDrive {
					dataPoints = append(dataPoints, float64(disk.FreeSpace)/1024/1024/1024)
					// Add time label every N points or for first/last
					if i == 0 || i == len(m.history.Snapshots)-1 ||
						snapshot.Timestamp.Sub(lastTime) > 12*time.Hour {
						timeLabels = append(timeLabels, snapshot.Timestamp.Format("02.01 15:04"))
						lastTime = snapshot.Timestamp
					} else {
						timeLabels = append(timeLabels, "")
					}
					break
				}
			}
		}

		if len(dataPoints) > 0 {
			// Caption with drive info
			caption := fmt.Sprintf("Drive %s: Current: %.1f GB",
				selectedDrive, dataPoints[len(dataPoints)-1])
			if len(dataPoints) > 1 {
				change := dataPoints[len(dataPoints)-1] - dataPoints[0]
				caption += fmt.Sprintf(", Change: %+.1f GB", change)
			}

			// Graph options
			opts := []asciigraph.Option{
				asciigraph.Height(height),
				asciigraph.Width(m.width - 10),
				asciigraph.Caption(caption),
			}

			// Add timestamps below the graph
			maxLen := 0
			for _, label := range timeLabels {
				if len(label) > maxLen {
					maxLen = len(label)
				}
			}

			// Draw graph
			graph := asciigraph.Plot(dataPoints, opts...)
			s.WriteString(graph)
			s.WriteString("\n")

			// Time axis
			pointWidth := (m.width - 10) / len(timeLabels)
			for i, label := range timeLabels {
				if label != "" {
					padding := strings.Repeat(" ", i*pointWidth)
					s.WriteString(fmt.Sprintf("%s%s", padding, label))
				}
			}
			s.WriteString("\n\n")

			// Stats
			var min, max, sum float64
			min = dataPoints[0]
			max = dataPoints[0]
			for _, v := range dataPoints {
				if v < min {
					min = v
				}
				if v > max {
					max = v
				}
				sum += v
			}
			avg := sum / float64(len(dataPoints))

			s.WriteString(fmt.Sprintf("Stats for period:\n"))
			s.WriteString(fmt.Sprintf("  Min: %.1f GB\n", min))
			s.WriteString(fmt.Sprintf("  Max: %.1f GB\n", max))
			s.WriteString(fmt.Sprintf("  Avg: %.1f GB\n", avg))
			s.WriteString(fmt.Sprintf("  Range: %.1f GB\n", max-min))
		}
	}

	// Drive legend
	s.WriteString("\nDrives: ")
	for i, drive := range drives {
		if i > 0 {
			s.WriteString("  ")
		}
		style := lipgloss.NewStyle().Foreground(lineColors[i%len(lineColors)])
		if i == m.selectedDisk {
			style = style.Bold(true).Underline(true)
		}
		s.WriteString(style.Render(drive))
	}

	return s.String()
}

// collectAndSave collects data and saves to history (CLI mode)
func collectAndSave() error {
	disks := getAllDisksInfo()
	if len(disks) == 0 {
		return fmt.Errorf("no drives found")
	}

	snapshot := Snapshot{
		Timestamp: time.Now(),
		Disks:     disks,
	}

	history, err := loadHistory()
	if err != nil {
		return err
	}

	history.Snapshots = append(history.Snapshots, snapshot)

	if err := saveHistory(history); err != nil {
		return err
	}

	fmt.Println("Disk data saved:")
	fmt.Printf("Time: %s\n", snapshot.Timestamp.Format("2006-01-02 15:04:05"))
	fmt.Println("----------------------------------------")
	for _, disk := range disks {
		fmt.Printf("Drive %s:\n", disk.Drive)
		fmt.Printf("  Total:     %s\n", formatBytes(disk.TotalSpace))
		fmt.Printf("  Free:      %s\n", formatBytes(disk.FreeSpace))
		fmt.Printf("  Used:      %s\n", formatBytes(disk.UsedSpace))
		fmt.Printf("  Used:      %.1f%%\n", float64(disk.UsedSpace)/float64(disk.TotalSpace)*100)
		fmt.Println()
	}

	return nil
}

func main() {
	showGraphFlag := flag.Bool("graph", false, "Show interactive graph")
	flag.Parse()

	if *showGraphFlag {
		// Run interactive mode
		p := tea.NewProgram(
			NewModel(),
			tea.WithAltScreen(),
		)

		if _, err := p.Run(); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
	} else {
		// Just collect and save data
		if err := collectAndSave(); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
	}
}
