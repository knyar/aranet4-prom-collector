package main

import (
	"embed"
	"fmt"
	"log/slog"
	"net/http"
	"time"
)

//go:embed index.html
var staticFiles embed.FS

func (c *collector) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}

	if r.Method == http.MethodPost {
		if err := r.ParseForm(); err != nil {
			http.Error(w, "Bad Request: failed to parse form", http.StatusBadRequest)
			return
		}

		action := r.FormValue("action")
		switch action {
		case "refresh":
			c.handleRefreshPost(w, r)
		case "passkey":
			c.handlePasskeyPost(w, r)
		default:
			http.Error(w, "Bad Request: unknown action", http.StatusBadRequest)
		}
		return
	}

	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	lastReported := c.lastReported.Load()
	var lastReportedAgo string
	if !lastReported.IsZero() {
		lastReportedAgo = formatDuration(time.Since(lastReported))
	}

	data := struct {
		LastReported    time.Time
		LastReportedAgo string
		WantPasskey     bool
	}{
		LastReported:    lastReported,
		LastReportedAgo: lastReportedAgo,
		WantPasskey:     c.passkeyChan.Load() != nil,
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := c.tmpl.Execute(w, data); err != nil {
		slog.Error("failed to execute template", "error", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
	}
}

// handleRefreshPost handles POST requests to trigger a refresh.
func (c *collector) handleRefreshPost(w http.ResponseWriter, r *http.Request) {
	select {
	case c.refreshChan <- true:
		slog.Info("refresh triggered via web interface")
	default:
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// handlePasskeyPost handles POST requests with passkey data for pairing.
func (c *collector) handlePasskeyPost(w http.ResponseWriter, r *http.Request) {
	passkeyStr := r.FormValue("passkey")
	if passkeyStr == "" {
		http.Error(w, "Bad Request: passkey is required", http.StatusBadRequest)
		return
	}

	var passkey int
	if _, err := fmt.Sscanf(passkeyStr, "%d", &passkey); err != nil {
		http.Error(w, "Bad Request: passkey must be a number", http.StatusBadRequest)
		return
	}

	// Send passkey to channel if it exists
	pkChan := c.passkeyChan.Load()
	if pkChan != nil {
		select {
		case pkChan <- passkey:
			slog.Info("passkey received via web interface")
			// Redirect back to the status page
			http.Redirect(w, r, "/", http.StatusSeeOther)
		case <-time.After(5 * time.Second):
			slog.Error("timeout sending passkey to channel")
			http.Error(w, "Internal Server Error: timeout sending passkey", http.StatusInternalServerError)
		}
	} else {
		http.Error(w, "Bad Request: no passkey request pending", http.StatusBadRequest)
	}
}

// formatDuration formats a duration into a human-readable string.
func formatDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%.0f seconds ago", d.Seconds())
	}
	if d < time.Hour {
		minutes := int(d.Minutes())
		if minutes == 1 {
			return "1 minute ago"
		}
		return fmt.Sprintf("%d minutes ago", minutes)
	}
	if d < 24*time.Hour {
		hours := int(d.Hours())
		if hours == 1 {
			return "1 hour ago"
		}
		return fmt.Sprintf("%d hours ago", hours)
	}
	days := int(d.Hours() / 24)
	if days == 1 {
		return "1 day ago"
	}
	return fmt.Sprintf("%d days ago", days)
}
