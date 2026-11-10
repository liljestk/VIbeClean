// Package schedule manages per-user Windows Task Scheduler jobs for Space Review.
package schedule

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unicode/utf16"

	"golang.org/x/sys/windows"

	"github.com/liljestk/VIbeClean/internal/model"
	"github.com/liljestk/VIbeClean/internal/units"
)

const descriptionPrefix = "Space Review scheduled capture "

type Config struct {
	Scope        string `json:"scope"`
	At           string `json:"at"`
	ThresholdGiB int64  `json:"threshold_gib"`
}

func (c Config) Validate() error {
	if c.Scope != model.QuickScope && c.Scope != model.DockerScope &&
		(!filepath.IsAbs(c.Scope) || model.VolumeRoot(c.Scope) == "") {
		return model.InvalidArgument("choose a local drive, folder, Quick review, or Docker")
	}
	at, err := time.Parse("15:04", c.At)
	if err != nil || at.Format("15:04") != c.At {
		return model.InvalidArgument("enter a local time such as 09:00 or 21:30")
	}
	if c.ThresholdGiB < 0 || c.ThresholdGiB > 1<<20 {
		return model.InvalidArgument("enter a whole number of GiB from 0 to 1048576")
	}
	return nil
}

func (c Config) Next(now time.Time) time.Time {
	at, err := time.Parse("15:04", c.At)
	if err != nil {
		return time.Time{}
	}
	next := time.Date(now.Year(), now.Month(), now.Day(), at.Hour(), at.Minute(), 0, 0, now.Location())
	if !next.After(now) {
		next = time.Date(now.Year(), now.Month(), now.Day()+1, at.Hour(), at.Minute(), 0, 0, now.Location())
	}
	return next
}

func (c Config) Encode() (string, error) {
	if err := c.Validate(); err != nil {
		return "", err
	}
	raw, err := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(raw), err
}

func Decode(encoded string) (Config, error) {
	var c Config
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return c, err
	}
	if err := json.Unmarshal(raw, &c); err != nil {
		return c, err
	}
	return c, c.Validate()
}

type Outcome struct {
	Attempted    int64    `json:"attempted_ms,omitempty"`
	LastSaved    int64    `json:"last_saved_ms,omitempty"`
	Result       string   `json:"result,omitempty"`
	ThresholdGiB int64    `json:"threshold_gib,omitempty"`
	LowVolumes   []string `json:"low_volumes,omitempty"`
	Alert        string   `json:"alert,omitempty"`
	AlertProblem string   `json:"alert_problem,omitempty"`
}

// LowSpace returns the newly low drives, and updates the set that stays low.
// A drive is rearmed after a later observation above the threshold.
func (o *Outcome) LowSpace(c Config, drives []model.DriveState) []model.DriveState {
	seen := make(map[string]bool)
	if o.ThresholdGiB == c.ThresholdGiB {
		for _, key := range o.LowVolumes {
			seen[key] = true
		}
	}
	o.ThresholdGiB, o.LowVolumes = c.ThresholdGiB, nil
	if c.ThresholdGiB == 0 {
		return nil
	}
	root := model.VolumeRoot(c.Scope)
	var crossings []model.DriveState
	for _, drive := range drives {
		if (root != "" && root != drive.Root) || drive.Total <= 0 || drive.Free < 0 ||
			drive.Free > c.ThresholdGiB*units.GiB {
			continue
		}
		key := drive.Root + "|" + drive.ID
		o.LowVolumes = append(o.LowVolumes, key)
		if !seen[key] {
			crossings = append(crossings, drive)
		}
	}
	return crossings
}

type State struct {
	Config     Config
	Registered bool
	Enabled    bool
	Problem    string
	Outcome    Outcome
}

type Manager struct {
	Executable string
	Index      string
}

func (m Manager) identity(scope string) (name, sid string, err error) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return "", "", err
	}
	sid = user.User.Sid.String()
	sum := sha256.Sum256([]byte(sid + "|" + model.NormalizedKey(m.Index) + "|" + model.NormalizedKey(scope)))
	return fmt.Sprintf("Space Review %x", sum[:12]), sid, nil
}

func scheduler(ctx context.Context, args ...string) ([]byte, error) {
	system, err := windows.GetSystemDirectory()
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, filepath.Join(system, "schtasks.exe"), args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_NO_WINDOW}
	return cmd.CombinedOutput()
}

type taskDefinition struct {
	XMLName     xml.Name     `xml:"Task"`
	Version     string       `xml:"version,attr"`
	Namespace   string       `xml:"xmlns,attr"`
	Description string       `xml:"RegistrationInfo>Description"`
	Start       string       `xml:"Triggers>CalendarTrigger>StartBoundary"`
	Days        int          `xml:"Triggers>CalendarTrigger>ScheduleByDay>DaysInterval"`
	Principal   principal    `xml:"Principals>Principal"`
	Settings    taskSettings `xml:"Settings"`
	Action      taskAction   `xml:"Actions"`
}

type principal struct {
	ID        string `xml:"id,attr"`
	UserID    string `xml:"UserId"`
	LogonType string `xml:"LogonType"`
	RunLevel  string `xml:"RunLevel"`
}

type taskSettings struct {
	Policy         string `xml:"MultipleInstancesPolicy"`
	BatteryBlocked bool   `xml:"DisallowStartIfOnBatteries"`
	BatteryStops   bool   `xml:"StopIfGoingOnBatteries"`
	IdleOnly       bool   `xml:"RunOnlyIfIdle"`
	IdleStops      bool   `xml:"IdleSettings>StopOnIdleEnd"`
	IdleRestarts   bool   `xml:"IdleSettings>RestartOnIdle"`
	WakeToRun      bool   `xml:"WakeToRun"`
	CatchUp        bool   `xml:"StartWhenAvailable"`
	Enabled        *bool  `xml:"Enabled,omitempty"`
	ExecutionLimit string `xml:"ExecutionTimeLimit"`
}

type taskAction struct {
	Context   string `xml:"Context,attr"`
	Command   string `xml:"Exec>Command"`
	Arguments string `xml:"Exec>Arguments"`
}

func (m Manager) definition(c Config, sid string, now time.Time) (taskDefinition, error) {
	encoded, err := c.Encode()
	if err != nil {
		return taskDefinition{}, err
	}
	return taskDefinition{
		Version: "1.2", Namespace: "http://schemas.microsoft.com/windows/2004/02/mit/task",
		Description: descriptionPrefix + encoded,
		Start:       c.Next(now).Format("2006-01-02T15:04:05"), Days: 1,
		Principal: principal{ID: "User", UserID: sid, LogonType: "InteractiveToken", RunLevel: "LeastPrivilege"},
		Settings: taskSettings{
			Policy: "IgnoreNew", CatchUp: true, ExecutionLimit: "PT4H",
			BatteryBlocked: true, BatteryStops: true, IdleOnly: true, IdleStops: true, IdleRestarts: true,
		},
		Action: taskAction{Context: "User", Command: m.Executable,
			Arguments: windows.ComposeCommandLine([]string{"--index", m.Index, "--scheduled", encoded})},
	}, nil
}

func taskText(raw []byte) []byte {
	if len(raw) >= 2 && ((raw[0] == 0xff && raw[1] == 0xfe) || raw[1] == 0) {
		units := make([]uint16, 0, len(raw)/2)
		for j := 0; j+1 < len(raw); j += 2 {
			units = append(units, binary.LittleEndian.Uint16(raw[j:]))
		}
		return []byte(strings.TrimPrefix(string(utf16.Decode(units)), "\ufeff"))
	}
	return bytes.TrimPrefix(raw, []byte{0xef, 0xbb, 0xbf})
}

func readDefinition(raw []byte) (taskDefinition, error) {
	var task taskDefinition
	decoder := xml.NewDecoder(bytes.NewReader(taskText(raw)))
	// schtasks can retain the original encoding declaration after conversion.
	decoder.CharsetReader = func(_ string, text io.Reader) (io.Reader, error) { return text, nil }
	err := decoder.Decode(&task)
	return task, err
}

func (m Manager) Read(ctx context.Context, scope string) (State, error) {
	state := State{Config: Config{Scope: scope, At: "09:00"}}
	name, _, err := m.identity(scope)
	if err != nil {
		return state, err
	}
	state.Outcome, err = m.ReadOutcome(scope)
	if err != nil {
		return state, err
	}
	raw, err := scheduler(ctx, "/Query", "/TN", name, "/XML")
	if err != nil {
		if ctx.Err() != nil {
			return state, ctx.Err()
		}
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 1 {
			return state, nil
		}
		return state, taskError("read schedule", raw, err)
	}
	task, err := readDefinition(raw)
	if err != nil {
		return state, err
	}
	encoded, ok := strings.CutPrefix(task.Description, descriptionPrefix)
	if !ok {
		return state, errors.New("Windows returned an unrecognized task for this scope")
	}
	state.Config, err = Decode(encoded)
	if err != nil {
		return state, err
	}
	state.Registered = true
	state.Enabled = task.Settings.Enabled == nil || *task.Settings.Enabled
	if _, err := os.Stat(task.Action.Command); err != nil {
		state.Problem = "The scheduled executable is unavailable. Save the schedule again from this copy."
	} else if !task.Settings.IdleOnly || !task.Settings.BatteryBlocked || !task.Settings.BatteryStops {
		state.Problem = "Save the schedule again to apply the idle and power conditions."
	}
	return state, nil
}

func (m Manager) Save(ctx context.Context, c Config) error {
	name, sid, err := m.identity(c.Scope)
	if err != nil {
		return err
	}
	task, err := m.definition(c, sid, time.Now())
	if err != nil {
		return err
	}
	raw, err := xml.MarshalIndent(task, "", "  ")
	if err != nil {
		return err
	}
	words := utf16.Encode([]rune("\ufeff<?xml version=\"1.0\" encoding=\"UTF-16\"?>\r\n" + string(raw)))
	xmlBytes := make([]byte, 0, 2*len(words))
	for _, word := range words {
		xmlBytes = binary.LittleEndian.AppendUint16(xmlBytes, word)
	}
	file, err := os.CreateTemp("", "space-review-task-*.xml")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	_, writeErr := file.Write(xmlBytes)
	if err := errors.Join(writeErr, file.Close()); err != nil {
		return err
	}
	output, err := scheduler(ctx, "/Create", "/TN", name, "/XML", file.Name(), "/F")
	return taskError("save schedule", output, err)
}

func (m Manager) Remove(ctx context.Context, scope string) error {
	name, _, err := m.identity(scope)
	if err != nil {
		return err
	}
	output, err := scheduler(ctx, "/Delete", "/TN", name, "/F")
	return taskError("remove schedule", output, err)
}

func taskError(action string, raw []byte, err error) error {
	if err == nil {
		return nil
	}
	if output := strings.TrimSpace(string(taskText(raw))); output != "" {
		return fmt.Errorf("could not %s: %s", action, output)
	}
	return fmt.Errorf("could not %s: %w", action, err)
}

func (m Manager) outcomePath(scope string) (string, error) {
	name, _, err := m.identity(scope)
	return filepath.Join(m.Index+".schedules", name+".json"), err
}

func (m Manager) ReadOutcome(scope string) (Outcome, error) {
	var state Outcome
	path, err := m.outcomePath(scope)
	if err != nil {
		return state, err
	}
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return state, nil
	}
	if err != nil {
		return state, err
	}
	return state, json.Unmarshal(raw, &state)
}

func (m Manager) WriteOutcome(scope string, state Outcome) error {
	path, err := m.outcomePath(scope)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".outcome-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	writeErr := json.NewEncoder(tmp).Encode(state)
	if err := errors.Join(writeErr, tmp.Close()); err != nil {
		return err
	}
	from, err := windows.UTF16PtrFromString(tmp.Name())
	if err != nil {
		return err
	}
	to, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	return windows.MoveFileEx(from, to, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH)
}
