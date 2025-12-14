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

	lastSuccess := c.lastSuccess.Load()
	lastReported := c.lastReported.Load()
	now := time.Now()

	// Calculate time ago strings
	var lastSuccessAgo, lastReportedAgo string
	if !lastSuccess.IsZero() {
		lastSuccessAgo = formatDuration(now.Sub(lastSuccess))
	}
	if !lastReported.IsZero() {
		lastReportedAgo = formatDuration(now.Sub(lastReported))
	}

	data := struct {
		LastSuccess     time.Time
		LastSuccessAgo  string
		LastReported    time.Time
		LastReportedAgo string
		CurrentTime     time.Time
	}{
		LastSuccess:     lastSuccess,
		LastSuccessAgo:  lastSuccessAgo,
		LastReported:    lastReported,
		LastReportedAgo: lastReportedAgo,
		CurrentTime:     now,
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := c.tmpl.Execute(w, data); err != nil {
		slog.Error("failed to execute template", "error", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
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
