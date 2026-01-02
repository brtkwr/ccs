package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"
)

const version = "0.1.0"

// Message represents a conversation message
type Message struct {
	Role string `json:"role"`
	Text string `json:"text"`
	Ts   string `json:"ts"`
}

// Conversation represents a parsed conversation
type Conversation struct {
	SessionID      string    `json:"session_id"`
	Cwd            string    `json:"cwd"`
	FirstTimestamp string    `json:"first_timestamp"`
	Messages       []Message `json:"messages"`
}

// RawMessage represents the JSON structure in conversation files
type RawMessage struct {
	Type    string `json:"type"`
	Cwd     string `json:"cwd"`
	Message struct {
		Content json.RawMessage `json:"content"`
	} `json:"message"`
	Timestamp string `json:"timestamp"`
}

// TextContent for parsing content arrays
type TextContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

func getProjectsDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude", "projects")
}

func extractText(content json.RawMessage) string {
	if len(content) == 0 {
		return ""
	}

	// Try as string first
	var str string
	if err := json.Unmarshal(content, &str); err == nil {
		return str
	}

	// Try as array
	var arr []TextContent
	if err := json.Unmarshal(content, &arr); err == nil {
		var parts []string
		for _, item := range arr {
			if item.Type == "text" && item.Text != "" {
				parts = append(parts, item.Text)
			}
		}
		return strings.Join(parts, " ")
	}

	return ""
}

func getConversations() ([]Conversation, error) {
	projectsDir := getProjectsDir()
	var conversations []Conversation

	err := filepath.Walk(projectsDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // Skip errors
		}

		if info.IsDir() || !strings.HasSuffix(path, ".jsonl") {
			return nil
		}

		if strings.HasPrefix(info.Name(), "agent-") {
			return nil
		}

		sessionID := strings.TrimSuffix(info.Name(), ".jsonl")
		conv := Conversation{SessionID: sessionID}

		file, err := os.Open(path)
		if err != nil {
			return nil
		}
		defer file.Close()

		scanner := bufio.NewScanner(file)
		scanner.Buffer(make([]byte, 1024*1024), 10*1024*1024) // 10MB buffer

		for scanner.Scan() {
			var raw RawMessage
			if err := json.Unmarshal(scanner.Bytes(), &raw); err != nil {
				continue
			}

			if raw.Type == "user" {
				if conv.Cwd == "" {
					conv.Cwd = raw.Cwd
				}
				text := extractText(raw.Message.Content)
				if strings.TrimSpace(text) != "" {
					if conv.FirstTimestamp == "" {
						conv.FirstTimestamp = raw.Timestamp
					}
					conv.Messages = append(conv.Messages, Message{
						Role: "user",
						Text: text,
						Ts:   raw.Timestamp,
					})
				}
			} else if raw.Type == "assistant" {
				text := extractText(raw.Message.Content)
				if strings.TrimSpace(text) != "" {
					conv.Messages = append(conv.Messages, Message{
						Role: "assistant",
						Text: text,
						Ts:   raw.Timestamp,
					})
				}
			}
		}

		if len(conv.Messages) > 0 {
			if conv.Cwd == "" {
				conv.Cwd = "unknown"
			}
			conversations = append(conversations, conv)
		}

		return nil
	})

	if err != nil {
		return nil, err
	}

	// Sort by timestamp descending
	sort.Slice(conversations, func(i, j int) bool {
		return conversations[i].FirstTimestamp > conversations[j].FirstTimestamp
	})

	return conversations, nil
}

func formatTimestamp(ts string) string {
	if ts == "" {
		return ""
	}
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		if len(ts) >= 16 {
			return ts[:16]
		}
		return ts
	}
	return t.Local().Format("2006-01-02 15:04")
}

func truncate(s string, maxLen int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen-3] + "..."
}

func buildSearchLines(conversations []Conversation) ([]string, map[string]Conversation) {
	var lines []string
	convMap := make(map[string]Conversation)

	for _, conv := range conversations {
		convMap[conv.SessionID] = conv
		project := conv.Cwd
		if idx := strings.LastIndex(conv.Cwd, "/"); idx >= 0 {
			project = conv.Cwd[idx+1:]
		}

		for i, msg := range conv.Messages {
			if msg.Role != "user" {
				continue
			}

			text := truncate(msg.Text, 100)
			ts := formatTimestamp(msg.Ts)

			line := fmt.Sprintf("%s:%d\t%s\t%s\t%s", conv.SessionID, i, ts, project, text)
			lines = append(lines, line)
		}
	}

	return lines, convMap
}

func highlight(text, query string) string {
	if query == "" {
		return text
	}
	re := regexp.MustCompile("(?i)" + regexp.QuoteMeta(query))
	return re.ReplaceAllStringFunc(text, func(match string) string {
		return fmt.Sprintf("\033[43;30m%s\033[0m", match)
	})
}

func formatCodeBlock(text, query, indent string) string {
	lines := strings.Split(text, "\n")
	var result []string
	inCodeBlock := false
	codeLang := ""

	for _, line := range lines {
		if strings.HasPrefix(line, "```") {
			if !inCodeBlock {
				inCodeBlock = true
				codeLang = strings.TrimPrefix(line, "```")
				if codeLang == "" {
					codeLang = "code"
				}
				result = append(result, fmt.Sprintf("%s\033[90m┌─ %s ─\033[0m", indent, codeLang))
			} else {
				inCodeBlock = false
				result = append(result, fmt.Sprintf("%s\033[90m└─────────\033[0m", indent))
			}
		} else if inCodeBlock {
			result = append(result, fmt.Sprintf("%s\033[90m│\033[0m \033[36m%s\033[0m", indent, line))
		} else {
			result = append(result, fmt.Sprintf("%s%s", indent, highlight(line, query)))
		}
	}

	return strings.Join(result, "\n")
}

func showPreview(line, query string, convMap map[string]Conversation) {
	parts := strings.Split(line, "\t")
	if len(parts) == 0 {
		return
	}

	sessionMsg := parts[0]
	lastColon := strings.LastIndex(sessionMsg, ":")
	if lastColon < 0 {
		return
	}

	sessionID := sessionMsg[:lastColon]
	var msgIdx int
	fmt.Sscanf(sessionMsg[lastColon+1:], "%d", &msgIdx)

	conv, ok := convMap[sessionID]
	if !ok {
		fmt.Println("Conversation not found")
		return
	}

	fmt.Printf("\033[1;33mProject:\033[0m %s\n", conv.Cwd)
	fmt.Printf("\033[1;33mSession:\033[0m %s\n", sessionID)
	fmt.Printf("\033[1;33mTotal messages:\033[0m %d\n\n", len(conv.Messages))

	start := msgIdx - 2
	if start < 0 {
		start = 0
	}
	end := msgIdx + 4
	if end > len(conv.Messages) {
		end = len(conv.Messages)
	}

	for i := start; i < end; i++ {
		msg := conv.Messages[i]
		var prefix string
		if i == msgIdx {
			if msg.Role == "user" {
				prefix = "\033[1;32m>>> User:\033[0m"
			} else {
				prefix = "\033[1;34m>>> Claude:\033[0m"
			}
		} else {
			if msg.Role == "user" {
				prefix = "\033[32m    User:\033[0m"
			} else {
				prefix = "\033[34m    Claude:\033[0m"
			}
		}

		text := msg.Text
		if len(text) > 2000 {
			text = text[:2000] + "\n... (truncated)"
		}

		fmt.Println(prefix)
		fmt.Println(formatCodeBlock(text, query, "    "))
		fmt.Println()
	}

	remaining := len(conv.Messages) - end
	if remaining > 0 {
		fmt.Printf("\033[90m    ... %d more messages\033[0m\n", remaining)
	}
}

func printHelp() {
	fmt.Printf(`ccs v%s - Claude Code Search

Search and resume Claude Code conversations using fzf.

Usage: ccs [flags]

Flags:
  -h, --help      Show this help message
  -v, --version   Show version

Any other flags are passed through to 'claude --resume'.

Examples:
  ccs                                 Search and resume a conversation
  ccs --dangerously-skip-permissions  Resume with auto-accept permissions
  ccs -y                              Same as above (short flag)

Requirements:
  - fzf (brew install fzf)
  - claude (Claude Code CLI)

`, version)
}

func main() {
	args := os.Args[1:]

	// Check for help/version
	for _, arg := range args {
		if arg == "-h" || arg == "--help" {
			printHelp()
			return
		}
		if arg == "-v" || arg == "--version" {
			fmt.Printf("ccs v%s\n", version)
			return
		}
	}

	// Internal preview mode
	if len(args) >= 2 && args[0] == "--preview" {
		line := args[1]
		query := ""
		if len(args) >= 3 {
			query = args[2]
		}

		conversations, err := getConversations()
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}

		_, convMap := buildSearchLines(conversations)
		showPreview(line, query, convMap)
		return
	}

	// Collect flags to pass to claude
	var claudeFlags []string
	for _, arg := range args {
		if strings.HasPrefix(arg, "-") {
			claudeFlags = append(claudeFlags, arg)
		}
	}

	// Check projects dir exists
	projectsDir := getProjectsDir()
	if _, err := os.Stat(projectsDir); os.IsNotExist(err) {
		fmt.Fprintf(os.Stderr, "Projects directory not found: %s\n", projectsDir)
		fmt.Fprintf(os.Stderr, "Make sure Claude Code is installed and has been used at least once.\n")
		os.Exit(1)
	}

	// Check fzf is installed
	if _, err := exec.LookPath("fzf"); err != nil {
		fmt.Fprintf(os.Stderr, "fzf not found. Install with: brew install fzf\n")
		os.Exit(1)
	}

	conversations, err := getConversations()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error loading conversations: %v\n", err)
		os.Exit(1)
	}

	if len(conversations) == 0 {
		fmt.Fprintf(os.Stderr, "No conversations found\n")
		os.Exit(1)
	}

	lines, convMap := buildSearchLines(conversations)
	if len(lines) == 0 {
		fmt.Fprintf(os.Stderr, "No searchable messages found\n")
		os.Exit(1)
	}

	// Get path to self for preview
	self, _ := os.Executable()

	// Run fzf
	cmd := exec.Command("fzf",
		"--ansi",
		"--delimiter=\t",
		"--with-nth=2,3,4",
		"--preview", fmt.Sprintf("%s --preview {} {q}", self),
		"--preview-window=right:60%:wrap",
		"--header=Search conversations | Enter to resume, Esc to quit",
		"--prompt=Search: ",
		"--height=90%",
		"--layout=reverse",
		"--border=rounded",
		"--info=inline",
	)

	cmd.Stdin = strings.NewReader(strings.Join(lines, "\n"))
	cmd.Stderr = os.Stderr

	output, err := cmd.Output()
	if err != nil {
		// User cancelled (exit code 130) or no selection
		return
	}

	selected := strings.TrimSpace(string(output))
	if selected == "" {
		return
	}

	parts := strings.Split(selected, "\t")
	sessionMsg := parts[0]
	lastColon := strings.LastIndex(sessionMsg, ":")
	sessionID := sessionMsg[:lastColon]

	conv, ok := convMap[sessionID]
	if !ok {
		fmt.Fprintf(os.Stderr, "Conversation not found\n")
		os.Exit(1)
	}

	cwd := conv.Cwd
	if cwd == "" || cwd == "unknown" {
		cwd = "."
	}

	fmt.Printf("\033[1mResuming conversation %s in %s...\033[0m\n", sessionID, cwd)
	if len(claudeFlags) > 0 {
		fmt.Printf("\033[90mFlags: %s\033[0m\n", strings.Join(claudeFlags, " "))
	}
	fmt.Println()

	// Change to project directory
	if err := os.Chdir(cwd); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: could not change to directory %s: %v\n", cwd, err)
	}

	// Exec claude
	claudePath, err := exec.LookPath("claude")
	if err != nil {
		fmt.Fprintf(os.Stderr, "claude not found in PATH\n")
		os.Exit(1)
	}

	execArgs := []string{"claude", "--resume", sessionID}
	execArgs = append(execArgs, claudeFlags...)

	syscall.Exec(claudePath, execArgs, os.Environ())
}
