package restic

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fakeRestic(t *testing.T, body string) string {
	t.Helper()
	name := filepath.Join(t.TempDir(), "restic")
	if err := os.WriteFile(name, []byte("#!/bin/sh\nset -eu\n"+body), 0o700); err != nil {
		t.Fatal(err)
	}
	return name
}

func TestSnapshotsArgumentsAndSorting(t *testing.T) {
	record := filepath.Join(t.TempDir(), "args")
	client := New(fakeRestic(t, `printf '%s\n' "$@" > "$RECORD"
printf '%s' '[{"time":"2024-01-01T00:00:00Z","id":"old"},{"time":"2025-01-01T00:00:00Z","id":"new"}]'
`), time.Second, 2, 1)
	t.Setenv("RECORD", record)
	snapshots, err := client.Snapshots(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if snapshots[0].ID != "new" {
		t.Fatalf("snapshots were not newest first: %#v", snapshots)
	}
	arguments, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	if string(arguments) != "snapshots\n--json\n" {
		t.Fatalf("unexpected arguments %q", arguments)
	}
}

func TestDirectoryJSONLAndSorting(t *testing.T) {
	client := New(fakeRestic(t, `printf '%s\n' \
'{"struct_type":"snapshot","id":"ignored"}' \
'{"struct_type":"node","name":"z.txt","type":"file","path":"/z.txt","size":3}' \
'{"struct_type":"node","name":"nested.txt","type":"file","path":"/dir/nested.txt"}' \
'{"struct_type":"node","name":"Alpha","type":"dir","path":"/Alpha"}' \
'{"struct_type":"node","name":"beta","type":"dir","path":"/beta"}'
`), time.Second, 2, 1)
	nodes, err := client.Directory(context.Background(), strings.Repeat("a", 64), "/")
	if err != nil {
		t.Fatal(err)
	}
	if got := []string{nodes[0].Name, nodes[1].Name, nodes[2].Name}; strings.Join(got, ",") != "Alpha,beta,z.txt" {
		t.Fatalf("unexpected order or depth filtering: %v", got)
	}
}

func TestMalformedOutput(t *testing.T) {
	client := New(fakeRestic(t, "printf '{bad json'\n"), time.Second, 1, 1)
	if _, err := client.Directory(context.Background(), strings.Repeat("a", 64), "/"); err == nil || !strings.Contains(err.Error(), "decode") {
		t.Fatalf("expected decode error, got %v", err)
	}
}

func TestMetadataTimeout(t *testing.T) {
	client := New(fakeRestic(t, "exec sleep 5\n"), 30*time.Millisecond, 1, 1)
	started := time.Now()
	_, err := client.Snapshots(context.Background())
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected deadline, got %v", err)
	}
	if time.Since(started) > time.Second {
		t.Fatal("timed out process was not promptly canceled")
	}
}

func TestRequestCancellationStopsMetadataProcess(t *testing.T) {
	client := New(fakeRestic(t, "exec sleep 5\n"), time.Minute, 1, 1)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	started := time.Now()
	_, err := client.Snapshots(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cancellation, got %v", err)
	}
	if time.Since(started) > time.Second {
		t.Fatal("canceled process was not promptly stopped")
	}
}

func TestCommandAndDownloadLimits(t *testing.T) {
	client := New(fakeRestic(t, "exec sleep 5\n"), time.Second, 1, 1)
	dump, err := client.Dump(context.Background(), strings.Repeat("a", 64), "/file")
	if err != nil {
		t.Fatal(err)
	}
	defer dump.Close()
	if _, err := client.Snapshots(context.Background()); !errors.Is(err, ErrBusy) {
		t.Fatalf("expected busy command limit, got %v", err)
	}
	if _, err := client.Dump(context.Background(), strings.Repeat("a", 64), "/other"); !errors.Is(err, ErrBusy) {
		t.Fatalf("expected busy download limit, got %v", err)
	}
}

func TestDumpStreamsAndWaits(t *testing.T) {
	client := New(fakeRestic(t, "printf payload\n"), time.Second, 1, 1)
	dump, err := client.Dump(context.Background(), strings.Repeat("a", 64), "/file")
	if err != nil {
		t.Fatal(err)
	}
	defer dump.Close()
	data, err := io.ReadAll(dump)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "payload" {
		t.Fatalf("unexpected dump %q", data)
	}
	if err := dump.Wait(); err != nil {
		t.Fatal(err)
	}
}

func TestMissingItemMappingAndBoundedStderr(t *testing.T) {
	client := New(fakeRestic(t, `printf 'does not exist: ' >&2
head -c 20000 /dev/zero | tr '\0' x >&2
exit 1
`), time.Second, 1, 1)
	_, err := client.Directory(context.Background(), strings.Repeat("a", 64), "/missing")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected not found, got %v", err)
	}
	if len(Diagnostic(err)) > stderrLimit+100 {
		t.Fatalf("stderr was not bounded: %d", len(Diagnostic(err)))
	}
}
