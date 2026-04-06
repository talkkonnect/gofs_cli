/*
 * gofs_cli a go implementationm of fs_cli for freeswitch
 * Copyright (C) 2026, Suvir Kumar <suvir@talkkonnect.com>
 *
 * This Source Code Form is subject to the terms of the Mozilla Public
 * License, v. 2.0. If a copy of the MPL was not distributed with this
 * file, You can obtain one at http://mozilla.org/MPL/2.0/.
 *
 * Software distributed under the License is distributed on an "AS IS" basis,
 * WITHOUT WARRANTY OF ANY KIND, either express or implied. See the License
 * for the specific language governing rights and limitations under the
 * License.
 *
 * gofs_cli is based on fs_cli.c from the freeswitch project
 *
 * The Initial Developer of the Original Code is
 * Suvir Kumar <suvir@talkkonnect.com> and some parts are vibe coded
 * Portions created by the Initial Developer are Copyright (C) Suvir Kumar. All Rights Reserved.
 *
 * Contributor(s):
 *
 * Suvir Kumar <suvir@talkkonnect.com>
 *
 */

package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"os/user"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/term"
)

const (
	ColorReset   = "\033[0m"
	ColorRed     = "\033[31m"
	ColorGreen   = "\033[32m"
	ColorYellow  = "\033[33m"
	ColorBlue    = "\033[34m"
	ColorMagenta = "\033[35m"
	ColorCyan    = "\033[36m"
	ColorGray    = "\033[90m"
	ClearLine    = "\033[2K\r"
)

var logLevelColors = map[string]string{
	"DEBUG":   ColorYellow,
	"INFO":    ColorGreen,
	"NOTICE":  ColorCyan,
	"WARNING": ColorMagenta,
	"ERROR":   ColorRed,
	"CRIT":    ColorRed,
	"ALERT":   ColorRed,
	"EMERG":   ColorRed,
}

var logLevelNames = []string{
	"WARNING", "NOTICE", "DEBUG", "ERROR", "ALERT", "EMERG", "CRIT", "INFO",
}

var fsLogTimePrefix = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}\s+\d{2}:\d{2}:\d{2}(\.\d+)?`)

func eslHeader(headers map[string]string, name string) string {
	for k, v := range headers {
		if strings.EqualFold(k, name) {
			return v
		}
	}
	return ""
}

func toTTYNewlines(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	return strings.ReplaceAll(s, "\n", "\r\n")
}

func formatLogLine(line string) string {
	line = strings.TrimRight(line, "\r\n")
	if line == "" {
		return ""
	}

	var tsPrefix, rest string
	if loc := fsLogTimePrefix.FindStringIndex(line); loc != nil {
		tsPrefix = line[loc[0]:loc[1]]
		rest = strings.TrimLeft(line[loc[1]:], " ")
	} else {
		tsPrefix = time.Now().Format("2006-01-02 15:04:05.000")
		rest = line
	}

	lineColor := ""
	for _, lvl := range logLevelNames {
		if strings.Contains(rest, "["+lvl+"]") {
			lineColor = logLevelColors[lvl]
			break
		}
	}

	full := tsPrefix + " " + rest
	if lineColor != "" {
		return lineColor + full + ColorReset
	}
	return ColorGray + tsPrefix + ColorReset + " " + rest
}

func formatLogData(body string) string {
	body = strings.ReplaceAll(body, "\r\n", "\n")
	body = strings.TrimRight(body, "\n")
	if body == "" {
		return ""
	}
	lines := strings.Split(body, "\n")
	for i := range lines {
		lines[i] = formatLogLine(lines[i])
	}
	return strings.Join(lines, "\n")
}

func getColorSequence(name string) string {
	switch strings.ToLower(name) {
	case "red": return ColorRed
	case "green": return ColorGreen
	case "yellow": return ColorYellow
	case "blue": return ColorBlue
	case "magenta": return ColorMagenta
	case "cyan": return ColorCyan
	case "gray", "grey": return ColorGray
	case "reset": return ColorReset
	default: return ColorReset
	}
}

type Config struct {
	Default  DefaultConfig `json:"default"`
	Profiles []Profile     `json:"profiles"`
}

type DefaultConfig struct {
	Loglevel int               `json:"loglevel"` // 1-7
	LogUUID  bool              `json:"log-uuid"`
	Keys     map[string]string `json:"keys"`
}

type Profile struct {
	Name           string `json:"name"`
	Host           string `json:"host"`
	Port           string `json:"port"`
	Password       string `json:"password"`
	Debug          int    `json:"debug"`
	Prompt         string `json:"prompt"`
	PromptColor    string `json:"prompt-color"`
	InputTextColor string `json:"input-text-color"`
}

var (
	conn         net.Conn
	mutex        sync.Mutex
	outputMu     sync.Mutex // serializes prompt + log writes (raw TTY is not concurrency-safe)
	inputBuffer  []rune
	history      []string
	historyIndex int

	activeConfig  Config
	activeProfile Profile
	terminalState *term.State

	// Common FS commands for Tab completion
	commands = []string{
		"status", "show channels", "show registrations", "show calls", "reloadxml", "sofia status",
		"version", "fsctl pause", "fsctl resume", "originate", "help",
		"/exit", "/quit", "/log", "/event", "/nolog", "...",
	}
)

func main() {
	loadConfig()
	
	profilePtr := flag.String("profile", "", "Profile to use")
	hostPtr := flag.String("H", "", "Host")
	portPtr := flag.String("P", "", "Port")
	passPtr := flag.String("p", "", "Password")
	flag.Parse()

	if *profilePtr != "" {
		found := false
		for _, p := range activeConfig.Profiles {
			if p.Name == *profilePtr {
				activeProfile = p
				found = true
				break
			}
		}
		if !found {
			fmt.Printf("Profile '%s' not found.\n", *profilePtr)
			return
		}
	}

	if *hostPtr != "" {
		activeProfile.Host = *hostPtr
	}
	if *portPtr != "" {
		activeProfile.Port = *portPtr
	}
	if *passPtr != "" {
		activeProfile.Password = *passPtr
	}

	// Dynamic IP calculation for prompt (if neither passed from JSON or fallbacks generated it)
	if activeProfile.Prompt == "" {
		activeProfile.Prompt = "gofs_cli " + getLocalIP()
	}

	address := net.JoinHostPort(activeProfile.Host, activeProfile.Port)

	var err error
	conn, err = net.DialTimeout("tcp", address, 5*time.Second)
	if err != nil {
		fmt.Printf("Connection failed: %v\n", err)
		return
	}
	defer conn.Close()

	reader := bufio.NewReaderSize(conn, 256*1024)
	if err := authenticate(reader, activeProfile.Password); err != nil {
		fmt.Printf("Auth failed: %v\n", err)
		return
	}

	// Switch terminal to Raw Mode to capture Tab, Arrows, and control the screen
	state, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err != nil {
		panic(err)
	}
	terminalState = state
	// Ensure we always restore the terminal state on exit
	defer term.Restore(int(os.Stdin.Fd()), terminalState)

	// Clean exit handler for Ctrl+C / SIGTERM
	go func() {
		c := make(chan os.Signal, 1)
		signal.Notify(c, os.Interrupt, syscall.SIGTERM)
		<-c
		term.Restore(int(os.Stdin.Fd()), terminalState)
		fmt.Print("\nExiting...\n")
		os.Exit(0)
	}()
	
	// Send initial log command dynamically based on config
	logLevelStr := "debug" // default fallback
	switch activeProfile.Debug {
	case 1: logLevelStr = "alert"
	case 2: logLevelStr = "crit"
	case 3: logLevelStr = "err"
	case 4: logLevelStr = "warning"
	case 5: logLevelStr = "notice"
	case 6: logLevelStr = "info"
	case 7: logLevelStr = "debug"
	}
	sendCommand(fmt.Sprintf("log %s\n\n", logLevelStr))

	if activeConfig.Default.LogUUID {
		sendCommand("log-uuid true\n\n")
	}

	// Async log reader
	go handleIncomingEvents(reader)

	// Run the interactive prompt
	runPrompt()
}

// writeLog ensures logs appear ABOVE the fixed bottom prompt
func writeLog(format string, a ...interface{}) {
	outputMu.Lock()
	defer outputMu.Unlock()
	fmt.Print(ClearLine)
	msg := fmt.Sprintf(format, a...)
	msg = toTTYNewlines(msg)
	if !strings.HasSuffix(msg, "\n") {
		msg += "\r\n"
	}
	fmt.Print(msg)
	renderPromptUnsafe()
}

func renderPromptUnsafe() {
	fmt.Print(ClearLine)
	promptCol := getColorSequence(activeProfile.PromptColor)
	textCol := getColorSequence(activeProfile.InputTextColor)
	fmt.Printf("%s%s>%s%s %s", promptCol, activeProfile.Prompt, ColorReset, textCol, string(inputBuffer))
}

func renderPrompt() {
	outputMu.Lock()
	defer outputMu.Unlock()
	renderPromptUnsafe()
}

func executeCommand(cmd string) {
	if cmd == "" {
		return
	}
	outputMu.Lock()
	fmt.Print("\r\n")
	outputMu.Unlock()
	
	if cmd == "..." || cmd == "/exit" || cmd == "/quit" || cmd == "/bye" {
		outputMu.Lock()
		fmt.Print(toTTYNewlines(fmt.Sprintf("\r\n%sThanks For Using gofscli Goodbye!%s\r\n\r\n", ColorYellow, ColorReset)))
		outputMu.Unlock()
		if terminalState != nil {
			term.Restore(int(os.Stdin.Fd()), terminalState)
		}
		os.Exit(0)
	}

	if len(history) == 0 || history[len(history)-1] != cmd {
		history = append(history, cmd)
	}
	historyIndex = len(history)

	if strings.HasPrefix(cmd, "/") {
		processSlashCommand(cmd)
	} else {
		sendCommand("api " + cmd + "\n\n")
	}
}

func handleFKey(key string) {
	cmd, ok := activeConfig.Default.Keys[key]
	if ok && cmd != "" {
		inputBuffer = []rune(cmd)
		renderPrompt()
		inputBuffer = nil
		executeCommand(cmd)
		renderPrompt()
	}
}

func handleEscapeSequence(seq string) {
	switch seq {
	case "[A", "OA": // Up Arrow
		if historyIndex > 0 {
			historyIndex--
			inputBuffer = []rune(history[historyIndex])
			renderPrompt()
		}
	case "[B", "OB": // Down Arrow
		if historyIndex < len(history)-1 {
			historyIndex++
			inputBuffer = []rune(history[historyIndex])
			renderPrompt()
		} else if historyIndex == len(history)-1 {
			historyIndex++
			inputBuffer = nil
			renderPrompt()
		}
	// F-Keys
	case "OP", "[11~": handleFKey("f1")
	case "OQ", "[12~": handleFKey("f2")
	case "OR", "[13~": handleFKey("f3")
	case "OS", "[14~": handleFKey("f4")
	case "[15~": handleFKey("f5")
	case "[17~": handleFKey("f6")
	case "[18~": handleFKey("f7")
	case "[19~": handleFKey("f8")
	case "[20~": handleFKey("f9")
	case "[21~": handleFKey("f10")
	case "[23~": handleFKey("f11")
	case "[24~": handleFKey("f12")
	}
}

func runPrompt() {
	renderPrompt()
	buf := make([]byte, 1)

	inEscape := false
	var escSeq string

	for {
		n, err := os.Stdin.Read(buf)
		if err != nil || n == 0 {
			break
		}

		char := buf[0]

		if char == 27 { // ESC
			inEscape = true
			escSeq = ""
			continue
		}
		if inEscape {
			escSeq += string(char)
			if (char >= 'A' && char <= 'Z') || (char >= 'a' && char <= 'z') || char == '~' {
				inEscape = false
				handleEscapeSequence(escSeq)
			}
			continue
		}

		switch char {
		case 3: // Ctrl+C
			return
		case 9: // Tab Key
			handleTab()
		case 13: // Enter
			cmd := strings.TrimSpace(string(inputBuffer))
			inputBuffer = nil
			executeCommand(cmd)
			renderPrompt()
		case 127, 8: // Backspace (127 standard, 8 Ctrl+H)
			if len(inputBuffer) > 0 {
				inputBuffer = inputBuffer[:len(inputBuffer)-1]
				renderPrompt()
			}
		default:
			if char >= 32 && char <= 126 { // Standard Printable Characters
				inputBuffer = append(inputBuffer, rune(char))
				renderPrompt()
			}
		}
	}
}

func handleTab() {
	current := string(inputBuffer)
	var matches []string
	for _, c := range commands {
		if strings.HasPrefix(c, current) {
			matches = append(matches, c)
		}
	}

	if len(matches) == 1 {
		inputBuffer = []rune(matches[0])
		renderPrompt()
	} else if len(matches) > 1 {
		outputMu.Lock()
		fmt.Print(toTTYNewlines("\r\n" + ClearLine + ColorCyan + strings.Join(matches, "  ") + ColorReset + "\r\n"))
		renderPromptUnsafe()
		outputMu.Unlock()
	}
}

func handleIncomingEvents(reader *bufio.Reader) {
	for {
		headers, body, err := readESLMessage(reader)
		if err != nil {
			if err != io.EOF {
				log.Printf("gofscli: ESL read error: %v", err)
			}
			return
		}

		contentType := eslHeader(headers, "Content-Type")
		if contentType == "api/response" || contentType == "command/reply" {
			color := ColorReset
			if strings.HasPrefix(body, "+OK") {
				color = ColorGreen
			} else if strings.HasPrefix(body, "-ERR") || strings.HasPrefix(body, "-USAGE") {
				color = ColorRed
			}
			writeLog("%s%s%s", color, strings.TrimSpace(body), ColorReset)
		} else if contentType == "log/data" {
			writeLog("%s", formatLogData(body))
		} else if contentType == "text/disconnect-notice" {
			writeLog("%sReceived disconnect notice. Press Enter to exit.%s", ColorRed, ColorReset)
		}
	}
}

func authenticate(reader *bufio.Reader, password string) error {
	headers, _, _ := readESLMessage(reader)
	if eslHeader(headers, "Content-Type") == "auth/request" {
		conn.Write([]byte(fmt.Sprintf("auth %s\n\n", password)))
		reply, _, _ := readESLMessage(reader)
		if strings.HasPrefix(eslHeader(reply, "Reply-Text"), "+OK") {
			return nil
		}
	}
	return fmt.Errorf("auth failed")
}

func readESLMessage(reader *bufio.Reader) (map[string]string, string, error) {
	headers := make(map[string]string)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return nil, "", err
		}
		line = strings.TrimSpace(line)
		if line == "" {
			break
		}
		idx := strings.IndexByte(line, ':')
		if idx < 0 {
			continue
		}
		key := strings.TrimSpace(line[:idx])
		val := strings.TrimSpace(line[idx+1:])
		if key != "" {
			headers[key] = val
		}
	}
	var body string
	if lengthStr := eslHeader(headers, "Content-Length"); lengthStr != "" {
		length, err := strconv.Atoi(strings.TrimSpace(lengthStr))
		if err != nil || length < 0 {
			return headers, "", fmt.Errorf("invalid Content-Length %q", lengthStr)
		}
		if length > 0 {
			bodyBytes := make([]byte, length)
			if _, err := io.ReadFull(reader, bodyBytes); err != nil {
				return headers, "", err
			}
			body = string(bodyBytes)
		}
	}
	return headers, body, nil
}

func sendCommand(cmd string) {
	mutex.Lock()
	defer mutex.Unlock()
	conn.Write([]byte(cmd))
}

func processSlashCommand(cmd string) {
	if strings.HasPrefix(cmd, "/log") {
		parts := strings.Fields(cmd)
		level := "debug"
		if len(parts) > 1 {
			level = parts[1]
		}
		log.Printf("BP 1 %s", level)
		sendCommand(fmt.Sprintf("log %s\n\n", level))
	} else if cmd == "/nolog" {
		sendCommand("nolog\n\n")
		log.Println("BP 2")
		} else if strings.HasPrefix(cmd, "/event") {
		parts := strings.Fields(cmd)
		log.Println("BP 3")
		if len(parts) > 1 {
			sendCommand(fmt.Sprintf("event %s\n\n", strings.Join(parts[1:], " ")))
			log.Println("BP 4")
		} else {
			sendCommand("event plain ALL\n\n")
			log.Println("BP 5")
		}
	} else if cmd == "/help" {
		writeLog("%s/help, /exit, /quit, /bye, ..., /log, /nolog, /event%s", ColorCyan, ColorReset)
	} else {
		writeLog("%sUnknown local command: %s%s", ColorRed, cmd, ColorReset)
	}
}

func loadConfig() {
	activeConfig = Config{
		Default: DefaultConfig{
			Loglevel: 7,
			LogUUID:  false,
			Keys: map[string]string{
				"f1":  "help",
				"f2":  "status",
				"f3":  "show channels",
				"f4":  "show calls",
				"f5":  "sofia status",
				"f6":  "reloadxml",
				"f7":  "/log console",
				"f8":  "/log debug",
				"f9":  "sofia status profile internal",
				"f10": "fsctl pause",
				"f11": "fsctl resume",
				"f12": "version",
			},
		},
	}

	paths := []string{"./gofs_cli.json", "/etc/gofs_cli.json"}
	if usr, err := user.Current(); err == nil {
		paths = append(paths, filepath.Join(usr.HomeDir, ".gofs_cli.json"))
	}

	for _, path := range paths {
		if data, err := os.ReadFile(path); err == nil {
			json.Unmarshal(data, &activeConfig)
			break
		}
	}

	if len(activeConfig.Profiles) > 0 {
		activeProfile = activeConfig.Profiles[0]
	} else {
		// Fallback profile if unspecified
		activeProfile = Profile{Host: "127.0.0.1", Port: "8021", Password: "ClueCon", Debug: activeConfig.Default.Loglevel}
	}
}

// getLocalIP returns the local LAN IP address of the server
func getLocalIP() string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return "127.0.0.1"
	}
	for _, address := range addrs {
		if ipnet, ok := address.(*net.IPNet); ok && !ipnet.IP.IsLoopback() {
			if ipnet.IP.To4() != nil {
				return ipnet.IP.String()
			}
		}
	}
	return "127.0.0.1"
}
