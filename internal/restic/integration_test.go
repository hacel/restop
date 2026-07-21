package restic

import (
	"archive/tar"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func runRestic(t *testing.T, executable string, args ...string) {
	t.Helper()
	command := exec.Command(executable, args...)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("restic %v: %v: %s", args, err, output)
	}
}

func TestIntegrationRepository(t *testing.T) {
	if os.Getenv("RESTOP_INTEGRATION") != "1" {
		t.Skip("set RESTOP_INTEGRATION=1 to run with a real restic binary")
	}
	executable, err := exec.LookPath("restic")
	if err != nil {
		t.Skip("restic is not installed")
	}
	root := t.TempDir()
	repository := filepath.Join(root, "repository")
	password := filepath.Join(root, "password")
	fixtures := filepath.Join(root, "fixtures")
	if err := os.MkdirAll(filepath.Join(fixtures, "nested"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(password, []byte("integration-test-password\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixtures, "nested", "hello.txt"), []byte("hello from restop\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("RESTIC_REPOSITORY", repository)
	t.Setenv("RESTIC_PASSWORD_FILE", password)
	runRestic(t, executable, "init")
	runRestic(t, executable, "backup", fixtures)

	// Exercise real JSON, JSONL, file streaming, and the tar layout emitted by restic.
	client := New(executable, time.Minute, 4, 2)
	snapshots, err := client.Snapshots(context.Background())
	if err != nil || len(snapshots) != 1 {
		t.Fatalf("snapshots: %v, %d found", err, len(snapshots))
	}
	selected, err := client.Snapshots(context.Background(), snapshots[0].ID)
	if err != nil || len(selected) != 1 || selected[0].ID != snapshots[0].ID {
		t.Fatalf("selected snapshot: %v, %#v", err, selected)
	}
	repositoryPath := filepath.ToSlash(filepath.Join(fixtures, "nested"))
	listing, err := client.Directory(context.Background(), snapshots[0].ID, repositoryPath)
	if err != nil || len(listing.Nodes) != 1 || listing.Nodes[0].Name != "hello.txt" {
		t.Fatalf("directory: %v, %#v", err, listing.Nodes)
	}
	matches, err := client.Search(context.Background(), snapshots[0].ID, "*.TXT")
	if err != nil || len(matches) != 1 || matches[0].Path != repositoryPath+"/hello.txt" {
		t.Fatalf("search: %v, %#v", err, matches)
	}
	file, err := client.Dump(context.Background(), snapshots[0].ID, repositoryPath+"/hello.txt")
	if err != nil {
		t.Fatal(err)
	}
	contents, err := io.ReadAll(file)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Wait(); err != nil {
		t.Fatal(err)
	}
	_ = file.Close()
	if string(contents) != "hello from restop\n" {
		t.Fatalf("unexpected file contents %q", contents)
	}
	directory, err := client.Dump(context.Background(), snapshots[0].ID, repositoryPath)
	if err != nil {
		t.Fatal(err)
	}
	archive := tar.NewReader(directory)
	found := false
	for {
		header, err := archive.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasSuffix(header.Name, "nested/hello.txt") {
			found = true
		}
	}
	if err := directory.Wait(); err != nil {
		t.Fatal(err)
	}
	_ = directory.Close()
	if !found {
		t.Fatal("directory archive did not contain nested/hello.txt")
	}
}
