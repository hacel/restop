package restic

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path"
	"sort"
	"strings"
	"sync"
	"time"
)

const stderrLimit = 8 << 10

var (
	ErrBusy     = errors.New("restic process limit reached")
	ErrNotFound = errors.New("repository item not found")
)

type Snapshot struct {
	Time     time.Time `json:"time"`
	Hostname string    `json:"hostname"`
	Paths    []string  `json:"paths"`
	Tags     []string  `json:"tags"`
	ID       string    `json:"id"`
	ShortID  string    `json:"short_id"`
	Summary  *struct {
		TotalBytesProcessed uint64 `json:"total_bytes_processed"`
	} `json:"summary"`
}

type Node struct {
	Name    string    `json:"name"`
	Type    string    `json:"type"`
	Path    string    `json:"path"`
	Size    uint64    `json:"size"`
	ModTime time.Time `json:"mtime"`
}

type commandError struct {
	message string
	stderr  string
	cause   error
}

func (e *commandError) Error() string {
	return e.message
}

func (e *commandError) Unwrap() error {
	return e.cause
}

func (e *commandError) Diagnostic() string {
	if e.stderr == "" {
		return e.message
	}
	return e.message + ": " + e.stderr
}

type limitedBuffer struct {
	buf bytes.Buffer
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if b.buf.Len() < stderrLimit {
		_, _ = b.buf.Write(p[:min(len(p), stderrLimit-b.buf.Len())])
	}
	return n, nil
}

func (b *limitedBuffer) String() string {
	return strings.TrimSpace(b.buf.String())
}

type Client struct {
	executable      string
	metadataTimeout time.Duration
	commands        chan struct{}
	downloads       chan struct{}
}

func New(executable string, metadataTimeout time.Duration, maxCommands, maxDownloads int) *Client {
	return &Client{
		executable:      executable,
		metadataTimeout: metadataTimeout,
		commands:        make(chan struct{}, maxCommands),
		downloads:       make(chan struct{}, maxDownloads),
	}
}

func acquire(ctx context.Context, semaphore chan struct{}) error {
	select {
	case semaphore <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	default:
		return ErrBusy
	}
}

func release(semaphore chan struct{}) {
	<-semaphore
}

func (c *Client) run(ctx context.Context, repository bool, args ...string) ([]byte, error) {
	if err := acquire(ctx, c.commands); err != nil {
		return nil, err
	}
	defer release(c.commands)

	// Metadata operations have a firm ceiling so abandoned requests cannot retain processes.
	ctx, cancel := context.WithTimeout(ctx, c.metadataTimeout)
	defer cancel()
	var stderr limitedBuffer
	cmd := exec.CommandContext(ctx, c.executable, args...)
	cmd.Stderr = &stderr
	output, err := cmd.Output()
	if err == nil {
		return output, nil
	}
	if ctx.Err() != nil {
		return nil, &commandError{message: "restic operation timed out or was canceled", stderr: stderr.String(), cause: ctx.Err()}
	}
	if errors.Is(err, exec.ErrNotFound) {
		return nil, &commandError{message: "restic executable was not found", cause: err}
	}
	if repository {
		if strings.Contains(strings.ToLower(stderr.String()), "not found") || strings.Contains(strings.ToLower(stderr.String()), "does not exist") {
			return nil, &commandError{message: "repository item was not found", stderr: stderr.String(), cause: errors.Join(ErrNotFound, err)}
		}
		return nil, &commandError{message: "restic could not read the repository", stderr: stderr.String(), cause: err}
	}
	return nil, &commandError{message: "restic command failed", stderr: stderr.String(), cause: err}
}

func decodeNodes(data []byte) ([]Node, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	var nodes []Node
	for {
		var value struct {
			StructType string `json:"struct_type"`
			Node
		}
		if err := decoder.Decode(&value); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return nil, fmt.Errorf("decode restic listing: %w", err)
		}
		if value.StructType == "snapshot" {
			continue
		}
		if value.Type == "" {
			return nil, errors.New("decode restic listing: node is missing required fields")
		}

		// Restic omits symlink targets from JSON listings, so expose only files and
		// directories the web interface can browse or download.
		if value.Type != "file" && value.Type != "dir" {
			continue
		}
		if value.Path == "" || value.Path != "/" && value.Name == "" {
			return nil, errors.New("decode restic listing: node is missing required fields")
		}
		nodes = append(nodes, value.Node)
	}
	return nodes, nil
}

func (c *Client) Snapshots(ctx context.Context) ([]Snapshot, error) {
	output, err := c.run(ctx, true, "snapshots", "--json")
	if err != nil {
		return nil, err
	}
	var snapshots []Snapshot
	if err := json.Unmarshal(output, &snapshots); err != nil {
		return nil, fmt.Errorf("decode restic snapshots: %w", err)
	}
	for _, snapshot := range snapshots {
		if snapshot.ID == "" || snapshot.Time.IsZero() {
			return nil, errors.New("decode restic snapshots: snapshot is missing required fields")
		}
	}
	sort.SliceStable(snapshots, func(i, j int) bool { return snapshots[i].Time.After(snapshots[j].Time) })
	return snapshots, nil
}

func (c *Client) Preflight(ctx context.Context) error {
	if _, err := c.run(ctx, false, "version"); err != nil {
		return err
	}
	if _, err := c.Snapshots(ctx); err != nil {
		return err
	}
	return nil
}

func (c *Client) Directory(ctx context.Context, snapshotID, repositoryPath string) ([]Node, error) {
	output, err := c.run(ctx, true, "ls", "--json", snapshotID, repositoryPath)
	if err != nil {
		return nil, err
	}
	nodes, err := decodeNodes(output)
	if err != nil {
		return nil, err
	}
	for _, node := range nodes {
		if node.Path == repositoryPath && node.Type != "dir" {
			return nil, ErrNotFound
		}
	}

	// Restic can return the selected node as well; expose only its direct children.
	children := nodes[:0]
	for _, node := range nodes {
		if node.Path != repositoryPath && path.Dir(node.Path) == repositoryPath {
			children = append(children, node)
		}
	}
	sort.SliceStable(children, func(i, j int) bool {
		if (children[i].Type == "dir") != (children[j].Type == "dir") {
			return children[i].Type == "dir"
		}
		left, right := strings.ToLower(children[i].Name), strings.ToLower(children[j].Name)
		if left == right {
			return children[i].Name < children[j].Name
		}
		return left < right
	})
	return children, nil
}

func (c *Client) Stat(ctx context.Context, snapshotID, repositoryPath string) (Node, error) {
	if repositoryPath == "/" {
		return Node{Name: "/", Type: "dir", Path: "/"}, nil
	}
	nodes, err := c.Directory(ctx, snapshotID, path.Dir(repositoryPath))
	if err != nil {
		return Node{}, err
	}
	for _, node := range nodes {
		if node.Path == repositoryPath {
			return node, nil
		}
	}
	return Node{}, ErrNotFound
}

type Dump struct {
	stdout   io.ReadCloser
	cmd      *exec.Cmd
	stderr   *limitedBuffer
	commands chan struct{}
	download chan struct{}
	once     sync.Once
}

func (d *Dump) Read(p []byte) (int, error) {
	return d.stdout.Read(p)
}

func (d *Dump) finish() {
	d.once.Do(func() {
		release(d.commands)
		release(d.download)
	})
}

func (d *Dump) Close() error {
	err := d.stdout.Close()
	if d.cmd.Process != nil {
		_ = d.cmd.Process.Kill()
	}
	_ = d.cmd.Wait()
	d.finish()
	return err
}

func (d *Dump) Wait() error {
	err := d.cmd.Wait()
	d.finish()
	if err != nil {
		return &commandError{message: "restic download failed", stderr: d.stderr.String(), cause: err}
	}
	return nil
}

func (c *Client) Dump(ctx context.Context, snapshotID, repositoryPath string) (*Dump, error) {
	if err := acquire(ctx, c.downloads); err != nil {
		return nil, err
	}
	if err := acquire(ctx, c.commands); err != nil {
		release(c.downloads)
		return nil, err
	}
	stderr := &limitedBuffer{}
	cmd := exec.CommandContext(ctx, c.executable, "dump", snapshotID, repositoryPath)
	cmd.Stderr = stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		release(c.commands)
		release(c.downloads)
		return nil, fmt.Errorf("prepare restic download: %w", err)
	}
	if err := cmd.Start(); err != nil {
		release(c.commands)
		release(c.downloads)
		return nil, &commandError{message: "restic download could not start", stderr: stderr.String(), cause: err}
	}
	return &Dump{stdout: stdout, cmd: cmd, stderr: stderr, commands: c.commands, download: c.downloads}, nil
}

func Diagnostic(err error) string {
	if commandErr, ok := errors.AsType[*commandError](err); ok {
		return commandErr.Diagnostic()
	}
	return err.Error()
}
