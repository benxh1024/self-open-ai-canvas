package hostupdate

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type recordingRunner struct {
	calls  [][]string
	inputs [][]byte
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func (r *recordingRunner) Run(ctx context.Context, name string, args, environment []string, stdout, stderr io.Writer) error {
	return r.RunInput(ctx, name, args, environment, nil, stdout, stderr)
}

func (r *recordingRunner) RunInput(_ context.Context, _ string, args, _ []string, input io.Reader, stdout, _ io.Writer) error {
	r.calls = append(r.calls, append([]string(nil), args...))
	if input != nil {
		data, err := io.ReadAll(input)
		if err != nil {
			return err
		}
		r.inputs = append(r.inputs, data)
	}
	if stdout != nil {
		_, _ = io.WriteString(stdout, "backup-fixture")
	}
	return nil
}

func TestSetEnvValuePreservesOtherSettings(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, ".env")
	if err := os.WriteFile(path, []byte("# keep\nCANVAS_IMAGE_TAG=1.0.0\nPOSTGRES_DB=canvas\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := setEnvValue(path, "CANVAS_IMAGE_TAG", "1.2.2-preview.1"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	value := string(data)
	if !strings.Contains(value, "# keep\n") || !strings.Contains(value, "POSTGRES_DB=canvas\n") || !strings.Contains(value, "CANVAS_IMAGE_TAG=1.2.2-preview.1\n") {
		t.Fatalf("unexpected env contents: %q", value)
	}
	stat, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if stat.Mode().Perm() != 0o640 {
		t.Fatalf("mode=%o, want 640", stat.Mode().Perm())
	}
}

func TestVerifyZipBackupRejectsCorruption(t *testing.T) {
	path := filepath.Join(t.TempDir(), "backup.zip")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	archive := zip.NewWriter(file)
	for name, content := range map[string]string{
		"metadata.json":    "{}",
		"database.dump":    "database",
		"backend-data.tar": "data",
	} {
		entry, createErr := archive.Create(name)
		if createErr != nil {
			t.Fatal(createErr)
		}
		if _, writeErr := io.WriteString(entry, content); writeErr != nil {
			t.Fatal(writeErr)
		}
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(data)
	checksum := "sha256:" + hex.EncodeToString(hash[:])
	if err := verifyZipBackup(path, checksum); err != nil {
		t.Fatalf("valid backup rejected: %v", err)
	}
	if err := os.WriteFile(path, append(data, byte(1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := verifyZipBackup(path, checksum); err == nil {
		t.Fatal("corrupted backup was accepted")
	}
}

func TestCurrentVersionRejectsLatest(t *testing.T) {
	installDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(installDir, ".env"), []byte("CANVAS_IMAGE_TAG=latest\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	manager := &Manager{config: Config{InstallDir: installDir, EnvFile: ".env"}}
	if _, err := manager.currentVersion(); err == nil {
		t.Fatal("latest tag was accepted")
	}
}

func TestNewManagerDefaultsToSelfOpenImageRepository(t *testing.T) {
	root := t.TempDir()
	manager, err := NewManager(Config{
		Repository: "benxh1024/self-open-ai-canvas",
		InstallDir: root,
		StateDir:   filepath.Join(root, "state"),
		BackupDir:  filepath.Join(root, "backups"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := manager.config.ImageRepository, "ghcr.io/benxh1024/self-open-ai-canvas"; got != want {
		t.Fatalf("ImageRepository=%q, want %q", got, want)
	}
	if manager.config.DatabaseMode != DatabaseModeManaged {
		t.Fatalf("DatabaseMode=%q, want managed", manager.config.DatabaseMode)
	}
}

func TestNewManagerRejectsUnknownDatabaseMode(t *testing.T) {
	_, err := NewManager(Config{DatabaseMode: DatabaseMode("shared")})
	if err == nil {
		t.Fatal("unknown database mode was accepted")
	}
}

func TestExternalBackupAndRestoreUseDBToolsAndPreserveDataMount(t *testing.T) {
	installDir := t.TempDir()
	backupDir := filepath.Join(installDir, "backups")
	if err := os.MkdirAll(backupDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(installDir, ".env"), []byte("DATABASE_URL=postgresql://canvas:secret@host.docker.internal:5432/canvas\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runner := &recordingRunner{}
	manager := &Manager{
		config: Config{
			InstallDir:           installDir,
			ComposeFile:          "docker-compose.external-services.yml",
			MigrationComposeFile: "docker-compose.external-migrate.yml",
			EnvFile:              ".env",
			BackupDir:            backupDir,
			DatabaseMode:         DatabaseModeExternal,
			StepTimeout:          time.Minute,
		},
		runner: runner,
	}
	backup, err := manager.createBackup("v1.2.2-preview.2")
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.restoreDatabase(backup); err != nil {
		t.Fatal(err)
	}
	if err := manager.restoreBackendData(backup); err != nil {
		t.Fatal(err)
	}

	var backupCall, restoreCall, dataRestoreCall string
	for _, call := range runner.calls {
		joined := strings.Join(call, " ")
		switch {
		case strings.Contains(joined, "db-tools -ec exec pg_dump"):
			backupCall = joined
		case strings.Contains(joined, "db-tools -ec exec pg_restore"):
			restoreCall = joined
		case strings.Contains(joined, "--user root --entrypoint sh backend -ec find /data"):
			dataRestoreCall = joined
		}
	}
	if !strings.Contains(backupCall, "-f "+filepath.Join(installDir, "docker-compose.external-migrate.yml")) {
		t.Fatalf("external backup did not use migration Compose: %q", backupCall)
	}
	if !strings.Contains(backupCall, `--dbname="$DATABASE_URL" --format=custom`) {
		t.Fatalf("external backup did not use DATABASE_URL: %q", backupCall)
	}
	if !strings.Contains(restoreCall, `--clean --if-exists --no-owner --no-privileges --dbname="$DATABASE_URL"`) {
		t.Fatalf("external restore did not clean the dedicated database: %q", restoreCall)
	}
	if !strings.Contains(dataRestoreCall, "find /data -mindepth 1 -maxdepth 1 -exec rm -rf -- {} + && tar -C /data -xf -") {
		t.Fatalf("backend data restore did not preserve /data root: %q", dataRestoreCall)
	}
	if len(runner.inputs) != 2 {
		t.Fatalf("input streams=%d, want database and data restore streams", len(runner.inputs))
	}
}

func TestCreateBackupReadsBackendDataAsRoot(t *testing.T) {
	installDir := t.TempDir()
	backupDir := filepath.Join(installDir, "backups")
	if err := os.MkdirAll(backupDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(installDir, ".env"), []byte("POSTGRES_USER=canvas\nPOSTGRES_DB=canvas\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runner := &recordingRunner{}
	manager := &Manager{
		config: Config{InstallDir: installDir, ComposeFile: "docker-compose.deploy.yml", EnvFile: ".env", BackupDir: backupDir},
		runner: runner,
	}
	if _, err := manager.createBackup("v1.2.2-preview.2"); err != nil {
		t.Fatal(err)
	}
	for _, call := range runner.calls {
		joined := strings.Join(call, " ")
		if strings.Contains(joined, "run --rm --no-deps --pull never -T --user root --entrypoint tar backend -C /data -cf - .") {
			return
		}
	}
	t.Fatalf("backend data backup did not use root: %#v", runner.calls)
}

func TestCheckWritableDirectory(t *testing.T) {
	directory := t.TempDir()
	if err := checkWritableDirectory(directory); err != nil {
		t.Fatalf("writable directory rejected: %v", err)
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("write probe was not cleaned up: %v", entries)
	}
	if err := checkWritableDirectory(filepath.Join(directory, "missing")); err == nil {
		t.Fatal("missing directory was accepted")
	}
}

func TestComposeProjectNameUsesInstallDirectory(t *testing.T) {
	cases := map[string]string{
		"/opt/yingce":           "yingce",
		"/srv/Open AI Canvas_2": "openaicanvas_2",
		"/":                     "open-ai-canvas",
		"/srv/!!!":              "open-ai-canvas",
	}
	for installDir, want := range cases {
		if got := composeProjectName(installDir); got != want {
			t.Errorf("composeProjectName(%q) = %q, want %q", installDir, got, want)
		}
	}
}

func TestWriteComposeEnvOverrideReplacesImageRefs(t *testing.T) {
	installDir := t.TempDir()
	stateDir := filepath.Join(installDir, "state")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	envPath := filepath.Join(installDir, ".env")
	if err := os.WriteFile(envPath, []byte("CANVAS_BACKEND_IMAGE=old-backend\nCANVAS_WEB_IMAGE=old-web\nPOSTGRES_DB=canvas\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	manager := &Manager{config: Config{InstallDir: installDir, EnvFile: ".env", StateDir: stateDir}}
	path, err := manager.writeComposeEnvOverride(deploymentImages{backend: "new-backend", web: "new-web", agent: "new-agent"})
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(path)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	value := string(data)
	if !strings.Contains(value, "CANVAS_BACKEND_IMAGE=new-backend\n") || !strings.Contains(value, "CANVAS_WEB_IMAGE=new-web\n") || !strings.Contains(value, "CANVAS_YINGCE_AGENT_IMAGE=new-agent\n") || !strings.Contains(value, "POSTGRES_DB=canvas\n") {
		t.Fatalf("unexpected override env: %q", value)
	}
}

// v1.5.8.x 的 Host Updater 只注入 backend/web 镜像，也不会生成 Agent Token。
// 部署 Compose 中 Agent 相关变量必须有回退值，否则旧更新器的预检会直接失败。
func TestDeployComposeAgentVariablesFallbackForLegacyUpdater(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "..", "docker-compose.deploy.yml"))
	if err != nil {
		t.Fatal(err)
	}
	compose := string(data)
	for _, required := range []string{"${CANVAS_YINGCE_AGENT_IMAGE:?", "${YINGCE_AGENT_TOKEN:?"} {
		if strings.Contains(compose, required) {
			t.Errorf("docker-compose.deploy.yml must not hard-require %s…}", required)
		}
	}
	for _, fallback := range []string{"${CANVAS_YINGCE_AGENT_IMAGE:-", "${YINGCE_AGENT_TOKEN:-${CANVAS_UPDATER_TOKEN:?"} {
		if !strings.Contains(compose, fallback) {
			t.Errorf("docker-compose.deploy.yml is missing fallback %s…}", fallback)
		}
	}
}
