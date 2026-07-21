package web

import (
	"context"
	"errors"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"restop/internal/restic"

	"github.com/dustin/go-humanize"
)

var snapshotIDPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

type breadcrumb struct {
	Name    string
	Path    string
	Current bool
}

type pageData struct {
	Title     string
	Snapshots []restic.Snapshot
	restic.Directory
	Path        string
	DisplayPath string
	Breadcrumbs []breadcrumb
	Status      int
	Heading     string
	Message     string
}

type Server struct {
	restic    *restic.Client
	logger    *slog.Logger
	snapshots *template.Template
	directory *template.Template
	errorPage *template.Template
	static    http.Handler
	requests  atomic.Uint64
}

func formatBytes(size uint64) string {
	if size < 1024 {
		return fmt.Sprintf("%d B", size)
	}
	units := []string{"KiB", "MiB", "GiB", "TiB", "PiB"}
	value := float64(size)
	for _, unit := range units {
		value /= 1024
		if value < 1024 || unit == units[len(units)-1] {
			return fmt.Sprintf("%.1f %s", value, unit)
		}
	}
	return strconv.FormatUint(size, 10) + " B"
}

func shortID(snapshot restic.Snapshot) string {
	if snapshot.ShortID == "" {
		return snapshot.ID[:min(8, len(snapshot.ID))]
	}
	return snapshot.ShortID[:min(8, len(snapshot.ShortID))]
}

func templateFunctions() template.FuncMap {
	return template.FuncMap{
		"bytes":     formatBytes,
		"localTime": humanize.Time,
		"queryPath": url.QueryEscape,
		"shortID":   shortID,
	}
}

func parsePage(name string) (*template.Template, error) {
	return template.New("base.html").Funcs(templateFunctions()).ParseFS(assets, "templates/base.html", "templates/"+name)
}

func New(client *restic.Client, logger *slog.Logger) (*Server, error) {
	snapshots, err := parsePage("snapshots.html")
	if err != nil {
		return nil, fmt.Errorf("parse snapshot template: %w", err)
	}
	directory, err := parsePage("directory.html")
	if err != nil {
		return nil, fmt.Errorf("parse directory template: %w", err)
	}
	errorPage, err := parsePage("error.html")
	if err != nil {
		return nil, fmt.Errorf("parse error template: %w", err)
	}
	staticFS, err := fs.Sub(assets, "static")
	if err != nil {
		return nil, fmt.Errorf("open static assets: %w", err)
	}
	return &Server{restic: client, logger: logger, snapshots: snapshots, directory: directory, errorPage: errorPage, static: http.FileServerFS(staticFS)}, nil
}

func cleanRepositoryPath(value string) (string, error) {
	if value == "" {
		return "/", nil
	}

	// Decode paths that retained an encoded layer after query parsing.
	value, err := url.PathUnescape(value)
	if err != nil {
		return "", errors.New("path contains invalid URL encoding")
	}

	// Validate the decoded path before passing it to restic.
	if strings.ContainsRune(value, 0) || !strings.HasPrefix(value, "/") || path.Clean(value) != value {
		return "", errors.New("path must be a cleaned absolute repository path")
	}
	if slices.Contains(strings.Split(value, "/"), "..") {
		return "", errors.New("path traversal is not allowed")
	}
	return value, nil
}

func validateSnapshotID(value string) error {
	if !snapshotIDPattern.MatchString(value) {
		return errors.New("snapshot ID must be a full 64-character lowercase hexadecimal ID")
	}
	return nil
}

func makeBreadcrumbs(directory restic.Directory, repositoryPath string) []breadcrumb {
	crumbs := []breadcrumb{{Name: shortID(directory.Snapshot), Path: "/", Current: repositoryPath == "/"}}
	if repositoryPath == "/" {
		return crumbs
	}
	parts := strings.Split(strings.TrimPrefix(repositoryPath, "/"), "/")
	for i, part := range parts {
		crumbs = append(crumbs, breadcrumb{Name: part, Path: "/" + strings.Join(parts[:i+1], "/"), Current: i == len(parts)-1})
	}
	return crumbs
}

func (s *Server) render(w http.ResponseWriter, page *template.Template, status int, data pageData) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := page.ExecuteTemplate(w, "base", data); err != nil {
		s.logger.Error("render response", "error", err)
	}
}

func (s *Server) renderError(w http.ResponseWriter, status int, heading, message string) {
	s.render(w, s.errorPage, status, pageData{Title: heading, Status: status, Heading: heading, Message: message})
}

func (s *Server) handleFailure(w http.ResponseWriter, r *http.Request, err error) {
	s.logger.Error("request failed", "request_id", r.Context().Value(requestIDKey{}), "error", err)
	if errors.Is(err, restic.ErrBusy) {
		w.Header().Set("Retry-After", "1")
		s.renderError(w, http.StatusServiceUnavailable, "Repository is busy", "Too many repository operations are active. Please try again shortly.")
		return
	}
	if errors.Is(err, restic.ErrNotFound) {
		s.renderError(w, http.StatusNotFound, "Not found", "The requested item does not exist in this snapshot.")
		return
	}
	if errors.Is(err, context.DeadlineExceeded) {
		s.renderError(w, http.StatusGatewayTimeout, "Repository timed out", "The repository did not respond in time.")
		return
	}
	s.renderError(w, http.StatusBadGateway, "Repository unavailable", "Restop could not read the repository. Check the server logs for details.")
}

func (s *Server) snapshotsHandler(w http.ResponseWriter, r *http.Request) {
	snapshots, err := s.restic.Snapshots(r.Context())
	if err != nil {
		s.handleFailure(w, r, err)
		return
	}
	s.render(w, s.snapshots, http.StatusOK, pageData{Title: "Snapshots", Snapshots: snapshots})
}

func (s *Server) directoryHandler(w http.ResponseWriter, r *http.Request) {
	if err := validateSnapshotID(r.PathValue("id")); err != nil {
		s.renderError(w, http.StatusBadRequest, "Invalid snapshot", err.Error())
		return
	}
	repositoryPath, err := cleanRepositoryPath(r.URL.Query().Get("path"))
	if err != nil {
		s.renderError(w, http.StatusBadRequest, "Invalid path", err.Error())
		return
	}
	directory, err := s.restic.Directory(r.Context(), r.PathValue("id"), repositoryPath)
	if err != nil {
		s.handleFailure(w, r, err)
		return
	}
	s.render(w, s.directory, http.StatusOK, pageData{
		Title: repositoryPath, Directory: directory, Path: repositoryPath,
		DisplayPath: repositoryPath, Breadcrumbs: makeBreadcrumbs(directory, repositoryPath),
	})
}

func downloadName(node restic.Node, snapshotID string) string {
	name := path.Base(node.Path)
	if node.Path == "/" {
		name = "repository"
	}
	if node.Type == "dir" {
		return name + "-" + snapshotID[:8] + ".tar"
	}
	return name
}

func (s *Server) downloadHandler(w http.ResponseWriter, r *http.Request) {
	if err := validateSnapshotID(r.PathValue("id")); err != nil {
		s.renderError(w, http.StatusBadRequest, "Invalid snapshot", err.Error())
		return
	}
	repositoryPath, err := cleanRepositoryPath(r.URL.Query().Get("path"))
	if err != nil {
		s.renderError(w, http.StatusBadRequest, "Invalid path", err.Error())
		return
	}
	node, err := s.restic.Stat(r.Context(), r.PathValue("id"), repositoryPath)
	if err != nil {
		s.handleFailure(w, r, err)
		return
	}
	if node.Type != "file" && node.Type != "dir" {
		s.renderError(w, http.StatusUnprocessableEntity, "Unsupported item", "Only regular files and directories can be downloaded.")
		return
	}
	dump, err := s.restic.Dump(r.Context(), r.PathValue("id"), repositoryPath)
	if err != nil {
		s.handleFailure(w, r, err)
		return
	}
	defer dump.Close()

	// Downloads bypass HTMX and stream restic output without buffering it in memory.
	if node.Type == "dir" {
		w.Header().Set("Content-Type", "application/x-tar")
	} else {
		w.Header().Set("Content-Type", "application/octet-stream")
	}
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": downloadName(node, r.PathValue("id"))}))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if _, err := io.Copy(w, dump); err != nil {
		s.logger.Error("stream download", "request_id", r.Context().Value(requestIDKey{}), "error", err)
		return
	}
	if err := dump.Wait(); err != nil {
		s.logger.Error("complete download", "request_id", r.Context().Value(requestIDKey{}), "error", err)
	}
}

func healthHandler(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, "ok\n")
}

type requestIDKey struct{}

type responseRecorder struct {
	http.ResponseWriter
	status int
}

func (r *responseRecorder) WriteHeader(status int) {
	if r.status == 0 {
		r.status = status
	}
	r.ResponseWriter.WriteHeader(status)
}

func (r *responseRecorder) Write(data []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return r.ResponseWriter.Write(data)
}

func (s *Server) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		requestID := s.requests.Add(1)
		r = r.WithContext(context.WithValue(r.Context(), requestIDKey{}, requestID))
		w.Header().Set("Content-Security-Policy", "default-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'; object-src 'none'")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		recorder := &responseRecorder{ResponseWriter: w}
		next.ServeHTTP(recorder, r)
		s.logger.Info("request", "request_id", requestID, "method", r.Method, "path", r.URL.Path, "status", recorder.status, "duration_ms", time.Since(start).Milliseconds())
	})
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /assets/", http.StripPrefix("/assets/", s.static))
	mux.HandleFunc("GET /healthz", healthHandler)
	mux.HandleFunc("GET /snapshots/{id}/download", s.downloadHandler)
	mux.HandleFunc("GET /snapshots/{id}", s.directoryHandler)
	mux.HandleFunc("GET /{$}", s.snapshotsHandler)
	return s.middleware(mux)
}
