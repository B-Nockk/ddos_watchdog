package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"syscall"
	"time"
)

type LogEntry struct {
	SourceIP  string    `json:"source_ip"`
	Timestamp time.Time `json:"timestamp"`
	Method    string    `json:"method"`
	Path      string    `json:"path"`
	Status    int       `json:"status"`
	RespSize  int       `json:"response_size"`
}

type rawLogLine struct {
	SourceIP  string `json:"source_ip"`
	Timestamp string `json:"timestamp"`
	Method    string `json:"method"`
	Path      string `json:"path"`
	Status    int    `json:"status"`
	RespSize  int    `json:"response_size"`
}

type logMonitor struct {
	logPath   string
	file      *os.File
	lastInode uint64
	handlers  []func(LogEntry)
}

func NewLogMonitor(logPath string) (*logMonitor, error) {
	info, err := os.Stat(logPath)
	if err != nil {
		return nil, fmt.Errorf("log path inaccessible: %w", err)
	}
	if info.IsDir() {
		return nil, fmt.Errorf("log path is a directory: %s", logPath)
	}
	return &logMonitor{
		logPath: logPath,
	}, nil
}

func (monitor *logMonitor) OnEntry(fn func(LogEntry)) {
	monitor.handlers = append(monitor.handlers, fn)
}

func (monitor *logMonitor) currentInode() (uint64, error) {
	info, err := monitor.file.Stat()
	if err != nil {
		return 0, err
	}

	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, nil
	}

	return stat.Ino, nil
}

func (monitor *logMonitor) openFile() error {
	f, err := os.Open(monitor.logPath)
	if err != nil {
		return err
	}

	monitor.file = f
	monitor.lastInode, _ = monitor.currentInode()
	return nil
}

func (monitor *logMonitor) hasRotated() (bool, error) {
	info, err := os.Stat(monitor.logPath)
	if err != nil {
		return false, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return false, nil
	}

	return stat.Ino != monitor.lastInode, nil
}

func (monitor *logMonitor) parseLine(raw string) *LogEntry {
	var r rawLogLine
	if err := json.Unmarshal([]byte(raw), &r); err != nil {
		return nil
	}

	ts, err := time.Parse(time.RFC3339, r.Timestamp)
	if err != nil {
		ts, err = time.Parse("02/Jan/2006:15:04:05 -0700", r.Timestamp)
		if err != nil {
			ts = time.Now()
		}
	}

	return &LogEntry{
		SourceIP:  r.SourceIP,
		Timestamp: ts,
		Method:    r.Method,
		Path:      r.Path,
		Status:    r.Status,
		RespSize:  r.RespSize,
	}
}

func (monitor *logMonitor) emit(entry LogEntry) {
	for _, fn := range monitor.handlers {
		fn(entry)
	}
}

func (monitor *logMonitor) Start(ctx context.Context) error {
	if err := monitor.openFile(); err != nil {
		return err
	}

	defer monitor.file.Close()

	if _, err := monitor.file.Seek(0, 2); err != nil {
		return err
	}

	reader := bufio.NewReader(monitor.file)

	for {
		select {
		case <-ctx.Done():
			return nil
		default:
		}

		line, err := reader.ReadString('\n')

		if err != nil {
			if rotated, _ := monitor.hasRotated(); rotated {
				monitor.file.Close()
				if err := monitor.openFile(); err != nil {
					return err
				}
				reader = bufio.NewReader(monitor.file)
			}
			time.Sleep(100 * time.Millisecond)
			continue
		}

		entry := monitor.parseLine(line)
		if entry == nil {
			continue
		}
		monitor.emit(*entry)
	}
}
