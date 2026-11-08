package parser

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"strconv"
	"strings"
	"time"
)

// ValidPoolStates defines the whitelist of known OpenZFS pool health states.
var ValidPoolStates = map[string]struct{}{
	"ONLINE":    {},
	"DEGRADED":  {},
	"FAULTED":   {},
	"OFFLINE":   {},
	"UNAVAIL":   {},
	"REMOVED":   {},
	"SUSPENDED": {},
}

// AppendStatusNote safely concatenates diagnostic messages without duplicating.
func AppendStatusNote(existing, note string) string {
	note = strings.TrimSpace(note)
	if note == "" {
		return existing
	}
	if existing == "" {
		return note
	}
	if strings.Contains(existing, note) {
		return existing
	}
	return existing + "; " + note
}

// ParseVdevErrorCount parses numeric OpenZFS error counter fields with binary multipliers.
func ParseVdevErrorCount(s string) (uint64, error) {
	s = strings.TrimSpace(s)
	if len(s) == 0 {
		return 0, errors.New("empty error field")
	}

	multipliers := map[byte]float64{
		'K': 1024,
		'k': 1024,
		'M': 1024 * 1024,
		'm': 1024 * 1024,
		'G': 1024 * 1024 * 1024,
		'g': 1024 * 1024 * 1024,
		'T': 1024 * 1024 * 1024 * 1024,
		't': 1024 * 1024 * 1024 * 1024,
		'P': 1024 * 1024 * 1024 * 1024 * 1024,
		'p': 1024 * 1024 * 1024 * 1024 * 1024,
		'E': 1024 * 1024 * 1024 * 1024 * 1024 * 1024,
		'e': 1024 * 1024 * 1024 * 1024 * 1024 * 1024,
	}

	last := s[len(s)-1]
	if mult, ok := multipliers[last]; ok {
		numStr := s[:len(s)-1]
		if len(numStr) == 0 {
			return 0, fmt.Errorf("missing numeric value: %s", s)
		}
		f, err := strconv.ParseFloat(numStr, 64)
		if err != nil {
			return 0, err
		}
		if math.IsInf(f, 0) || math.IsNaN(f) {
			return 0, fmt.Errorf("non-finite numeric value: %s", s)
		}
		if f < 0 {
			return 0, fmt.Errorf("negative error count: %s", s)
		}
		if f*mult >= float64(math.MaxUint64) {
			return 0, fmt.Errorf("overflow: %s", s)
		}
		return uint64(f * mult), nil
	}

	return strconv.ParseUint(s, 10, 64)
}

// ParseScrubDate extracts and parses the completion timestamp from an OpenZFS scan/scrub line.
func ParseScrubDate(line string) (time.Time, error) {
	idx := strings.Index(line, " on ")
	if idx == -1 {
		return time.Time{}, errors.New("no ' on ' delimiter in scrub line")
	}
	dateStr := strings.TrimSpace(line[idx+4:])
	layouts := []string{
		"Mon Jan _2 15:04:05 2006",
		"Mon Jan  2 15:04:05 2006",
		"Mon Jan 02 15:04:05 2006",
		"2006-01-02 15:04:05",
		"2006-01-02 15:04:05 MST",
		"2006-01-02 15:04:05 -0700",
		"Mon Jan _2 15:04:05 2006 MST",
		time.ANSIC,
		time.UnixDate,
		time.RFC1123,
		time.RFC3339,
	}
	for _, layout := range layouts {
		if t, err := time.ParseInLocation(layout, dateStr, time.Local); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("unable to parse scrub date %q", dateStr)
}

// AddSaturatingUint64 prevents wraparound overflow when summing error counters.
func AddSaturatingUint64(a, b uint64) uint64 {
	if math.MaxUint64-a < b {
		return math.MaxUint64
	}
	return a + b
}

// IsVirtualContainer determines whether a vdev name represents a virtual aggregation group.
func IsVirtualContainer(vdevName, poolName string) bool {
	if vdevName == poolName {
		return true
	}
	prefixes := []string{
		"mirror", "raidz", "draid", "replacing", "spare",
		"special", "dedup", "logs", "cache",
	}
	for _, p := range prefixes {
		if strings.HasPrefix(vdevName, p) {
			return true
		}
	}
	return false
}

// MapVdevState normalizes leaf device states and maps degraded/faulted devices.
func MapVdevState(state string) string {
	switch strings.ToUpper(strings.TrimSpace(state)) {
	case "ONLINE", "AVAIL":
		return "ONLINE"
	case "DEGRADED", "OFFLINE", "UNAVAIL", "REMOVED":
		return "DEGRADED"
	case "FAULTED":
		return "DEGRADED"
	default:
		return ""
	}
}

// ParseResult encapsulates the structured output of parsing `zpool status`.
type ParseResult struct {
	PoolState        string
	ReadErrors       uint64
	WriteErrors      uint64
	ChecksumErrors   uint64
	LastScrubTime    time.Time
	HasLastScrub     bool
	NoScrubRequested bool
	ScrubInProgress  bool
	StatusNote       string
}

// StateSeverity defines the severity rank of pool states (higher = worse/more critical).
var StateSeverity = map[string]int{
	"UNKNOWN":   0,
	"ONLINE":    1,
	"DEGRADED":  2,
	"OFFLINE":   3,
	"UNAVAIL":   4,
	"FAULTED":   5,
	"REMOVED":   5,
	"SUSPENDED": 5,
}

// RaiseState elevates the parsed pool state if the new state represents a higher severity.
// When newState is UNKNOWN, it resets an unverified ONLINE state to UNKNOWN.
func (r *ParseResult) RaiseState(newState string) {
	if r.PoolState == "" {
		r.PoolState = "UNKNOWN"
	}
	if newState == "UNKNOWN" {
		if r.PoolState == "ONLINE" {
			r.PoolState = "UNKNOWN"
		}
		return
	}
	if StateSeverity[newState] > StateSeverity[r.PoolState] {
		r.PoolState = newState
	}
}

// ParseZpoolStatusReader processes raw zpool status output from an io.Reader.
func ParseZpoolStatusReader(r io.Reader, poolName string) (ParseResult, error) {
	res := ParseResult{
		PoolState: "UNKNOWN",
	}

	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 4096), 1024*1024)

	inConfig := false
	inHeader := false
	inSpares := false

	type vdevFrame struct {
		indent      int
		vdevName    string
		vdevState   string
		fields      []string
		hasChildren bool
	}

	var vdevStack []vdevFrame

	popFrame := func(f vdevFrame) {
		mapped := MapVdevState(f.vdevState)

		if f.vdevName == poolName {
			if _, ok := ValidPoolStates[f.vdevState]; ok {
				res.RaiseState(f.vdevState)
			} else {
				res.RaiseState("FAULTED")
			}
			if len(f.fields) < 5 && f.vdevState != "ONLINE" {
				res.StatusNote = AppendStatusNote(res.StatusNote, fmt.Sprintf("vdev %s is %s", f.vdevName, f.vdevState))
			}
			return
		}

		if mapped != "" && mapped != "ONLINE" {
			res.RaiseState(mapped)
		}

		if len(f.fields) < 5 {
			if mapped != "ONLINE" {
				res.StatusNote = AppendStatusNote(res.StatusNote, fmt.Sprintf("vdev %s is %s", f.vdevName, f.vdevState))
			}
			return
		}

		// Leaf devices (devices without children): parse error counters
		if !f.hasChildren {
			if r, err := ParseVdevErrorCount(f.fields[2]); err == nil {
				res.ReadErrors = AddSaturatingUint64(res.ReadErrors, r)
			} else {
				res.StatusNote = AppendStatusNote(res.StatusNote, fmt.Sprintf("unparseable error field %s: %s", f.fields[2], err))
			}

			if w, err := ParseVdevErrorCount(f.fields[3]); err == nil {
				res.WriteErrors = AddSaturatingUint64(res.WriteErrors, w)
			} else {
				res.StatusNote = AppendStatusNote(res.StatusNote, fmt.Sprintf("unparseable error field %s: %s", f.fields[3], err))
			}

			if k, err := ParseVdevErrorCount(f.fields[4]); err == nil {
				res.ChecksumErrors = AddSaturatingUint64(res.ChecksumErrors, k)
			} else {
				res.StatusNote = AppendStatusNote(res.StatusNote, fmt.Sprintf("unparseable error field %s: %s", f.fields[4], err))
			}
		}
	}

	flushStackDownTo := func(targetIndent int) {
		for len(vdevStack) > 0 && vdevStack[len(vdevStack)-1].indent >= targetIndent {
			top := vdevStack[len(vdevStack)-1]
			vdevStack = vdevStack[:len(vdevStack)-1]
			popFrame(top)
		}
	}

	flushEntireStack := func() {
		for len(vdevStack) > 0 {
			top := vdevStack[len(vdevStack)-1]
			vdevStack = vdevStack[:len(vdevStack)-1]
			popFrame(top)
		}
	}

	var statusParts []string
	var actionParts []string
	currentSection := ""

	for scanner.Scan() {
		rawLine := scanner.Text()
		line := strings.TrimSpace(rawLine)
		if line == "" {
			continue
		}

		if strings.HasPrefix(line, "state:") {
			if inConfig {
				flushEntireStack()
				inConfig = false
			}
			currentSection = ""
			parts := strings.Fields(line)
			if len(parts) >= 2 {
				st := strings.ToUpper(strings.TrimSpace(parts[1]))
				if _, ok := ValidPoolStates[st]; ok {
					res.RaiseState(st)
				} else {
					slog.Warn("Unexpected ZFS pool state from zpool status",
						"pool", poolName,
						"value", parts[1],
					)
					res.RaiseState("FAULTED")
					res.StatusNote = AppendStatusNote(res.StatusNote, fmt.Sprintf("unexpected pool state: %s", parts[1]))
				}
			}
		} else if strings.HasPrefix(line, "status:") {
			if inConfig {
				flushEntireStack()
				inConfig = false
			}
			currentSection = "status"
			val := strings.TrimSpace(strings.TrimPrefix(line, "status:"))
			if val != "" {
				statusParts = append(statusParts, strings.Join(strings.Fields(val), " "))
			}
		} else if strings.HasPrefix(line, "action:") {
			if inConfig {
				flushEntireStack()
				inConfig = false
			}
			currentSection = "action"
			val := strings.TrimSpace(strings.TrimPrefix(line, "action:"))
			if val != "" {
				actionParts = append(actionParts, strings.Join(strings.Fields(val), " "))
			}
		} else if strings.HasPrefix(line, "see:") {
			if inConfig {
				flushEntireStack()
				inConfig = false
			}
			currentSection = ""
		} else if strings.HasPrefix(line, "scan:") {
			if inConfig {
				flushEntireStack()
				inConfig = false
			}
			currentSection = ""
			if strings.Contains(line, "scrub in progress") || strings.Contains(line, "resilver in progress") {
				res.ScrubInProgress = true
				res.StatusNote = AppendStatusNote(res.StatusNote, "ZFS scan/scrub actively in progress")
			} else if strings.Contains(line, "scrub repaired") || strings.Contains(line, "resilvered") {
				if t, err := ParseScrubDate(line); err == nil {
					res.LastScrubTime = t
					res.HasLastScrub = true
				} else {
					slog.Warn("Failed to parse scrub completion date", "pool", poolName, "line", line, "error", err)
					res.StatusNote = AppendStatusNote(res.StatusNote, fmt.Sprintf("unparseable scrub date: %s", line))
				}
			} else if strings.Contains(line, "none requested") {
				res.NoScrubRequested = true
				res.StatusNote = AppendStatusNote(res.StatusNote, "No scrub has been performed on pool yet")
			}
		} else if strings.HasPrefix(line, "config:") {
			currentSection = ""
			inConfig = true
			inHeader = false
			inSpares = false
		} else if strings.HasPrefix(line, "errors:") {
			flushEntireStack()
			currentSection = ""
			inConfig = false
			inHeader = false
			inSpares = false
			errDetails := strings.TrimSpace(strings.TrimPrefix(line, "errors:"))
			if errDetails != "" && !strings.EqualFold(errDetails, "No known data errors") {
				res.RaiseState("FAULTED")
				res.StatusNote = AppendStatusNote(res.StatusNote, line)
			}
		} else if inConfig {
			expanded := strings.ReplaceAll(rawLine, "\t", "        ")
			indent := len(expanded) - len(strings.TrimLeft(expanded, " "))
			fields := strings.Fields(line)

			if len(fields) >= 2 && fields[0] == "NAME" && fields[1] == "STATE" {
				flushEntireStack()
				inHeader = true
				inSpares = false
				continue
			}

			if len(fields) == 1 {
				flushEntireStack()
				tag := strings.ToLower(strings.TrimSuffix(fields[0], ":"))
				inSpares = (tag == "spares")
				continue
			}

			if inHeader && len(fields) >= 2 {
				vdevName := fields[0]
				vdevState := fields[1]

				if inSpares {
					continue
				}

				mapped := MapVdevState(vdevState)
				if mapped == "" {
					continue
				}

				flushStackDownTo(indent)
				if len(vdevStack) > 0 {
					vdevStack[len(vdevStack)-1].hasChildren = true
				}
				vdevStack = append(vdevStack, vdevFrame{
					indent:    indent,
					vdevName:  vdevName,
					vdevState: vdevState,
					fields:    fields,
				})
			}
		} else {
			switch currentSection {
			case "status":
				statusParts = append(statusParts, strings.Join(strings.Fields(line), " "))
			case "action":
				actionParts = append(actionParts, strings.Join(strings.Fields(line), " "))
			}
		}
	}

	flushEntireStack()

	if len(statusParts) > 0 {
		res.StatusNote = AppendStatusNote(res.StatusNote, "status: "+strings.Join(statusParts, " "))
	}
	if len(actionParts) > 0 {
		res.StatusNote = AppendStatusNote(res.StatusNote, "action: "+strings.Join(actionParts, " "))
	}

	if err := scanner.Err(); err != nil {
		res.PoolState = "FAULTED"
		res.StatusNote = AppendStatusNote(res.StatusNote, fmt.Sprintf("scanner error: %v", err))
		return res, fmt.Errorf("scanner error for pool %s: %w", poolName, err)
	}

	return res, nil
}
