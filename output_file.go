package main

import (
	"bufio"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

var dateFileNameFuncs = map[string]func(*FileOutput) string{
	"%Y":  func(o *FileOutput) string { return time.Now().Format("2006") },
	"%m":  func(o *FileOutput) string { return time.Now().Format("01") },
	"%d":  func(o *FileOutput) string { return time.Now().Format("02") },
	"%H":  func(o *FileOutput) string { return time.Now().Format("15") },
	"%M":  func(o *FileOutput) string { return time.Now().Format("04") },
	"%S":  func(o *FileOutput) string { return time.Now().Format("05") },
	"%NS": func(o *FileOutput) string { return fmt.Sprint(time.Now().Nanosecond()) },
	"%r":  func(o *FileOutput) string { return string(o.currentID) },
	"%t":  func(o *FileOutput) string { return string(o.payloadType) },
}

// FileOutputConfig ...
type FileOutputConfig struct {
	flushInterval     time.Duration
	sizeLimit         int64
	outputFileMaxSize int64
	queueLimit        int
	append            bool
}

// FileOutput output plugin
type FileOutput struct {
	sync.RWMutex
	pathTemplate   string
	currentName    string
	file           *os.File
	queueLength    int
	chunkSize      int
	writer         io.Writer
	requestPerFile bool
	currentID      []byte
	payloadType    []byte
	closed         bool
	totalFileSize  int64

	config *FileOutputConfig
}

// NewFileOutput constructor for FileOutput, accepts path
func NewFileOutput(pathTemplate string, config *FileOutputConfig) *FileOutput {
	o := new(FileOutput)
	o.pathTemplate = pathTemplate
	o.config = config
	o.updateName()

	if strings.Contains(pathTemplate, "%r") {
		o.requestPerFile = true
	}

	go func() {
		for {
			time.Sleep(config.flushInterval)
			if o.IsClosed() {
				break
			}
			o.updateName()
			o.flush()
		}
	}()

	return o
}

func getFileIndex(name string) int {
	ext := filepath.Ext(name)
	withoutExt := strings.TrimSuffix(name, ext)

	if idx := strings.LastIndex(withoutExt, "_"); idx != -1 {
		if i, err := strconv.Atoi(withoutExt[idx+1:]); err == nil {
			return i
		}
	}

	return -1
}

func setFileIndex(name string, idx int) string {
	idxS := strconv.Itoa(idx)
	ext := filepath.Ext(name)
	withoutExt := strings.TrimSuffix(name, ext)

	if i := strings.LastIndex(withoutExt, "_"); i != -1 {
		if _, err := strconv.Atoi(withoutExt[i+1:]); err == nil {
			withoutExt = withoutExt[:i]
		}
	}

	return withoutExt + "_" + idxS + ext
}

func withoutIndex(s string) string {
	if i := strings.LastIndex(s, "_"); i != -1 {
		return s[:i]
	}

	return s
}

type sortByFileIndex []string

func (s sortByFileIndex) Len() int {
	return len(s)
}

func (s sortByFileIndex) Swap(i, j int) {
	s[i], s[j] = s[j], s[i]
}

func (s sortByFileIndex) Less(i, j int) bool {
	if withoutIndex(s[i]) == withoutIndex(s[j]) {
		return getFileIndex(s[i]) < getFileIndex(s[j])
	}

	return s[i] < s[j]
}

func (o *FileOutput) filename() string {
	o.RLock()
	defer o.RUnlock()

	path := o.pathTemplate

	for name, fn := range dateFileNameFuncs {
		path = strings.Replace(path, name, fn(o), -1)
	}

	if !o.config.append {
		nextChunk := false

		if o.currentName == "" ||
			((o.config.queueLimit > 0 && o.queueLength >= o.config.queueLimit) ||
				(o.config.sizeLimit > 0 && o.chunkSize >= int(o.config.sizeLimit))) {
			nextChunk = true
		}

		ext := filepath.Ext(path)
		withoutExt := strings.TrimSuffix(path, ext)

		if matches, err := filepath.Glob(withoutExt + "*" + ext); err == nil {
			if len(matches) == 0 {
				return setFileIndex(path, 0)
			}
			sort.Sort(sortByFileIndex(matches))

			last := matches[len(matches)-1]

			fileIndex := 0
			if idx := getFileIndex(last); idx != -1 {
				fileIndex = idx

				if nextChunk {
					fileIndex++
				}
			}

			return setFileIndex(last, fileIndex)
		}
	}

	return path
}

func (o *FileOutput) updateName() {
	name := filepath.Clean(o.filename())
	o.Lock()
	o.currentName = name
	o.Unlock()
}

func (o *FileOutput) Write(data []byte) (n int, err error) {
	if o.requestPerFile {
		o.Lock()
		meta := payloadMeta(data)
		o.currentID = meta[1]
		o.payloadType = meta[0]
		o.Unlock()
	}

	o.updateName()
	o.Lock()
	defer o.Unlock()

	if o.file == nil || o.currentName != o.file.Name() {
		o.closeLocked()

		o.file, err = os.OpenFile(o.currentName, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0660)
		o.file.Sync()

		if strings.HasSuffix(o.currentName, ".gz") {
			o.writer = gzip.NewWriter(o.file)
		} else {
			o.writer = bufio.NewWriter(o.file)
		}

		if err != nil {
			log.Fatal(o, "Cannot open file %q. Error: %s", o.currentName, err)
		}

		o.queueLength = 0
	}

	o.writer.Write(data)
	o.writer.Write([]byte(payloadSeparator))

	o.totalFileSize += int64(len(data) + len(payloadSeparator))
	o.queueLength++

	if Settings.outputFileConfig.outputFileMaxSize > 0 && o.totalFileSize >= Settings.outputFileConfig.outputFileMaxSize {
		return len(data), errors.New("File output reached size limit")
	}

	return len(data), nil
}

func (o *FileOutput) flush() {
	// Don't exit on panic
	defer func() {
		if r := recover(); r != nil {
			log.Println("PANIC while file flush: ", r, o, string(debug.Stack()))
		}
	}()

	o.Lock()
	defer o.Unlock()

	if o.file != nil {
		if strings.HasSuffix(o.currentName, ".gz") {
			o.writer.(*gzip.Writer).Flush()
		} else {
			o.writer.(*bufio.Writer).Flush()
		}

		if stat, err := o.file.Stat(); err == nil {
			o.chunkSize = int(stat.Size())
		} else {
			log.Println("Error accessing file sats", err)
		}
	}
}

func (o *FileOutput) String() string {
	return "File output: " + o.file.Name()
}

func (o *FileOutput) closeLocked() error {
	if o.file != nil {
		if strings.HasSuffix(o.currentName, ".gz") {
			o.writer.(*gzip.Writer).Close()
		} else {
			o.writer.(*bufio.Writer).Flush()
		}
		o.file.Close()
	}

	o.closed = true
	return nil
}

// Close closes the output file that is being written to.
func (o *FileOutput) Close() error {
	o.Lock()
	defer o.Unlock()
	return o.closeLocked()
	return nil
}

// IsClosed returns if the output file is closed or not.
func (o *FileOutput) IsClosed() bool {
	o.Lock()
	defer o.Unlock()
	return o.closed
}
