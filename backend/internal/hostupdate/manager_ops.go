package hostupdate

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

type githubRelease struct {
	TagName     string    `json:"tag_name"`
	Name        string    `json:"name"`
	Body        string    `json:"body"`
	HTMLURL     string    `json:"html_url"`
	PublishedAt time.Time `json:"published_at"`
	Draft       bool      `json:"draft"`
	Prerelease  bool      `json:"prerelease"`
}

type githubContent struct {
	Content  string `json:"content"`
	Encoding string `json:"encoding"`
}

type githubReleaseAsset struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

type githubReleaseDetails struct {
	Assets []githubReleaseAsset `json:"assets"`
}

func (m *Manager) latestRelease(ctx context.Context, sources Sources) (*Release, error) {
	endpoint := "https://api.github.com/repos/" + m.config.Repository + "/releases?per_page=30"
	response, err := m.githubGet(ctx, endpoint, sources, "application/vnd.github+json")
	if err != nil {
		return nil, fmt.Errorf("请求 GitHub Release：%w", err)
	}
	defer response.Body.Close()
	var releases []githubRelease
	decoder := json.NewDecoder(io.LimitReader(response.Body, maxReleaseResponseBytes))
	if err := decoder.Decode(&releases); err != nil {
		return nil, fmt.Errorf("解析 GitHub Release：%w", err)
	}
	filtered := make([]githubRelease, 0, len(releases))
	for _, release := range releases {
		if !release.Draft && strings.HasPrefix(release.TagName, "v") && IsReleaseVersion(release.TagName) {
			filtered = append(filtered, release)
		}
	}
	if len(filtered) == 0 {
		return nil, errors.New("GitHub 尚未发布可用 Release")
	}
	sort.SliceStable(filtered, func(i, j int) bool { return CompareVersions(filtered[i].TagName, filtered[j].TagName) > 0 })
	latest := filtered[0]
	return &Release{Version: latest.TagName, Name: latest.Name, Body: latest.Body, URL: latest.HTMLURL, PublishedAt: latest.PublishedAt, Prerelease: latest.Prerelease}, nil
}

func (m *Manager) currentVersion() (string, error) {
	values, err := readEnvFile(m.envPath())
	if err != nil {
		return "", err
	}
	version := strings.TrimSpace(values["CANVAS_IMAGE_TAG"])
	if version == "" || version == "latest" {
		return "", errors.New("CANVAS_IMAGE_TAG 必须固定为发布版本，不能使用 latest")
	}
	if !strings.HasPrefix(version, "v") {
		version = "v" + version
	}
	return version, nil
}

func (m *Manager) prepareTargetCompose(targetVersion string, sources Sources) (string, error) {
	return m.prepareTargetComposeFile(targetVersion, m.config.ComposeFile, sources)
}

func (m *Manager) prepareTargetMigrationCompose(targetVersion string, sources Sources) (string, error) {
	if m.config.MigrationComposeFile == m.config.ComposeFile {
		return m.prepareTargetCompose(targetVersion, sources)
	}
	return m.prepareTargetComposeFile(targetVersion, m.config.MigrationComposeFile, sources)
}

func (m *Manager) prepareTargetComposeFile(targetVersion, composeFile string, sources Sources) (string, error) {
	endpoint := fmt.Sprintf(
		"https://api.github.com/repos/%s/contents/%s?ref=%s",
		m.config.Repository,
		url.PathEscape(composeFile),
		url.QueryEscape(targetVersion),
	)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	response, err := m.githubGet(ctx, endpoint, sources, "application/vnd.github+json")
	if err != nil {
		return "", fmt.Errorf("下载目标 Compose：%w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("下载目标 Compose 返回 HTTP %d", response.StatusCode)
	}
	var content githubContent
	if err := json.NewDecoder(io.LimitReader(response.Body, 3<<20)).Decode(&content); err != nil {
		return "", fmt.Errorf("解析目标 Compose：%w", err)
	}
	if content.Encoding != "base64" {
		return "", fmt.Errorf("目标 Compose 编码不受支持：%s", content.Encoding)
	}
	data, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(strings.ReplaceAll(content.Content, "\n", ""), "\r", ""))
	if err != nil {
		return "", fmt.Errorf("解码目标 Compose：%w", err)
	}
	if len(data) == 0 {
		return "", errors.New("目标 Compose 文件为空")
	}
	if len(data) > 2<<20 {
		return "", errors.New("目标 Compose 文件超过允许大小")
	}
	name := strings.NewReplacer("/", "-", "\\", "-", ".", "-").Replace(composeFile)
	path := filepath.Join(m.config.StateDir, "compose-"+name+"-"+strings.TrimPrefix(targetVersion, "v")+".next.yml")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return "", fmt.Errorf("保存目标 Compose：%w", err)
	}
	return path, nil
}

func (m *Manager) preflight(composePath, targetVersion string, sources Sources) error {
	if _, err := os.Stat(m.composePath()); err != nil {
		return fmt.Errorf("读取当前 Compose：%w", err)
	}
	if _, err := os.Stat(m.envPath()); err != nil {
		return fmt.Errorf("读取部署环境：%w", err)
	}
	if m.config.SelfUpdate {
		if strings.TrimSpace(m.config.BinaryPath) == "" {
			return errors.New("检查 Host Updater 安装目录：二进制路径为空")
		}
		if err := checkWritableDirectory(filepath.Dir(m.config.BinaryPath)); err != nil {
			return fmt.Errorf("检查 Host Updater 安装目录：%w", err)
		}
	}
	if err := checkBackupDiskSpace(m.config.BackupDir); err != nil {
		return err
	}
	if m.config.DatabaseMode == DatabaseModeExternal {
		if _, err := os.Stat(m.migrationComposePath()); err != nil {
			return fmt.Errorf("读取当前迁移 Compose：%w", err)
		}
	}
	if err := m.composeWithImages(composePath, targetVersion, sources.images(m.config.ImageRepository, targetVersion), 2*time.Minute, nil, "config", "--quiet"); err != nil {
		return fmt.Errorf("目标 Compose 校验失败：%w", err)
	}
	current, err := m.currentVersion()
	if err != nil {
		return err
	}
	if err := m.checkHealthOnce(m.healthURL(), current); err != nil {
		return fmt.Errorf("当前运行版本与部署配置不一致或服务未就绪：%w", err)
	}
	return nil
}

func (m *Manager) preflightMigration(composePath, targetVersion string, sources Sources) error {
	if err := m.composeWithImages(composePath, targetVersion, sources.images(m.config.ImageRepository, targetVersion), 2*time.Minute, nil, "config", "--quiet"); err != nil {
		return fmt.Errorf("目标迁移 Compose 校验失败：%w", err)
	}
	return nil
}

func checkWritableDirectory(directory string) error {
	directory = strings.TrimSpace(directory)
	if directory == "" {
		return errors.New("Host Updater 二进制路径为空")
	}
	temporary, err := os.CreateTemp(directory, ".updater-write-test-*")
	if err != nil {
		return err
	}
	name := temporary.Name()
	if closeErr := temporary.Close(); closeErr != nil {
		_ = os.Remove(name)
		return closeErr
	}
	if removeErr := os.Remove(name); removeErr != nil {
		return removeErr
	}
	return nil
}

func (m *Manager) prepareUpdaterBinary(targetVersion string, sources Sources) (string, error) {
	if !m.config.SelfUpdate {
		return "", nil
	}
	if runtime.GOOS != "linux" || (runtime.GOARCH != "amd64" && runtime.GOARCH != "arm64") {
		return "", fmt.Errorf("Host Updater 自更新不支持 %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	if strings.TrimSpace(m.config.BinaryPath) == "" {
		return "", errors.New("未配置 Host Updater 二进制路径")
	}
	asset := "open-ai-canvas-host-updater-linux-" + runtime.GOARCH
	assetIDs, err := m.releaseAssetIDs(targetVersion, []string{"SHA256SUMS", asset}, sources)
	if err != nil {
		return "", fmt.Errorf("读取目标 Release 资产：%w", err)
	}
	checksums, err := m.downloadReleaseAssetByID(assetIDs["SHA256SUMS"], 1<<20, sources)
	if err != nil {
		return "", fmt.Errorf("下载 Host Updater 校验清单：%w", err)
	}
	expected, err := releaseChecksum(checksums, asset)
	if err != nil {
		return "", err
	}
	binary, err := m.downloadReleaseAssetByID(assetIDs[asset], 128<<20, sources)
	if err != nil {
		return "", fmt.Errorf("下载目标 Host Updater：%w", err)
	}
	hash := sha256.Sum256(binary)
	actual := hex.EncodeToString(hash[:])
	if actual != expected {
		return "", fmt.Errorf("Host Updater 校验失败：期望 %s，实际 %s", expected, actual)
	}
	path := filepath.Join(m.config.StateDir, asset+"-"+strings.TrimPrefix(targetVersion, "v")+".next")
	if err := os.WriteFile(path, binary, 0o700); err != nil {
		return "", err
	}
	return path, nil
}

func (m *Manager) releaseAssetIDs(targetVersion string, names []string, sources Sources) (map[string]int64, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	endpoint := fmt.Sprintf(
		"https://api.github.com/repos/%s/releases/tags/%s",
		m.config.Repository,
		url.PathEscape(targetVersion),
	)
	response, err := m.githubGet(ctx, endpoint, sources, "application/vnd.github+json")
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	var details githubReleaseDetails
	if err := json.NewDecoder(io.LimitReader(response.Body, maxReleaseResponseBytes)).Decode(&details); err != nil {
		return nil, err
	}
	wanted := make(map[string]struct{}, len(names))
	for _, name := range names {
		wanted[name] = struct{}{}
	}
	assets := make(map[string]int64, len(names))
	for _, asset := range details.Assets {
		if _, ok := wanted[asset.Name]; ok && asset.ID > 0 {
			assets[asset.Name] = asset.ID
		}
	}
	for _, name := range names {
		if assets[name] == 0 {
			return nil, fmt.Errorf("目标 Release %s 中不存在资产 %s", targetVersion, name)
		}
	}
	return assets, nil
}

func (m *Manager) downloadReleaseAsset(url string, limit int64, sources Sources) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	response, err := m.githubGet(ctx, url, sources, "")
	return readReleaseAsset(response, err, limit)
}

func (m *Manager) downloadReleaseAssetByID(assetID int64, limit int64, sources Sources) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	endpoint := fmt.Sprintf("https://api.github.com/repos/%s/releases/assets/%d", m.config.Repository, assetID)
	response, err := m.githubGet(ctx, endpoint, sources, "application/octet-stream")
	return readReleaseAsset(response, err, limit)
}

func readReleaseAsset(response *http.Response, err error, limit int64) ([]byte, error) {
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	reader := io.LimitReader(response.Body, limit+1)
	data, err := io.ReadAll(reader)
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, errors.New("Release 资产超过允许大小")
	}
	return data, nil
}

func releaseChecksum(data []byte, asset string) (string, error) {
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || strings.TrimPrefix(fields[1], "*") != asset {
			continue
		}
		digest, err := hex.DecodeString(fields[0])
		if err == nil && len(digest) == sha256.Size {
			return strings.ToLower(fields[0]), nil
		}
		break
	}
	return "", fmt.Errorf("目标 Release 的 SHA256SUMS 缺少有效的 %s 校验值", asset)
}

func (m *Manager) restartSelf() {
	time.Sleep(time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_ = m.runner.Run(ctx, "systemctl", []string{"restart", m.config.ServiceName}, nil, io.Discard, io.Discard)
}

type deploymentImages struct {
	backend string
	web     string
	agent   string
}

func (m *Manager) immutableImageRefs(version string) deploymentImages {
	return immutableImageRefs(m.config.ImageRepository, version)
}

func immutableImageRefs(repository, version string) deploymentImages {
	tag := strings.TrimPrefix(version, "v")
	return deploymentImages{
		backend: repository + "-backend:" + tag,
		web:     repository + "-web:" + tag,
		agent:   repository + "-yingce-agent:" + tag,
	}
}

func (m *Manager) compose(composePath, imageTag string, timeout time.Duration, stdout io.Writer, arguments ...string) error {
	return m.composeWithImagesInput(composePath, imageTag, deploymentImages{}, timeout, nil, stdout, arguments...)
}

func (m *Manager) composeWithImages(composePath, imageTag string, images deploymentImages, timeout time.Duration, stdout io.Writer, arguments ...string) error {
	return m.composeWithImagesInput(composePath, imageTag, images, timeout, nil, stdout, arguments...)
}

func (m *Manager) composeWithInput(composePath, imageTag string, timeout time.Duration, input io.Reader, stdout io.Writer, arguments ...string) error {
	return m.composeWithImagesInput(composePath, imageTag, deploymentImages{}, timeout, input, stdout, arguments...)
}

func (m *Manager) composeWithImagesInput(composePath, imageTag string, images deploymentImages, timeout time.Duration, input io.Reader, stdout io.Writer, arguments ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	envFile := m.envPath()
	if images.backend != "" {
		var err error
		envFile, err = m.writeComposeEnvOverride(images)
		if err != nil {
			return fmt.Errorf("准备目标 Compose 环境：%w", err)
		}
		defer os.Remove(envFile)
	}
	args := []string{"compose", "--project-name", composeProjectName(m.config.InstallDir), "--env-file", envFile, "-f", composePath}
	args = append(args, arguments...)
	var stderr bytes.Buffer
	if stdout == nil {
		stdout = io.Discard
	}
	environment := []string{"CANVAS_IMAGE_TAG=" + strings.TrimPrefix(imageTag, "v")}
	// Shell 环境优先于 --env-file，必须显式覆盖，防止已导出的旧镜像绕过所选来源。
	values, err := readEnvFile(envFile)
	if err != nil {
		return err
	}
	for _, key := range []string{"CANVAS_BACKEND_IMAGE", "CANVAS_WEB_IMAGE", "CANVAS_YINGCE_AGENT_IMAGE"} {
		if value := values[key]; value != "" {
			environment = append(environment, key+"="+value)
		}
	}
	err = m.runner.RunInput(ctx, "docker", args, environment, input, stdout, &stderr)
	if err != nil {
		message := strings.TrimSpace(stderr.String())
		if len(message) > 1000 {
			message = message[len(message)-1000:]
		}
		if message != "" {
			return fmt.Errorf("docker compose %s：%s", strings.Join(arguments, " "), message)
		}
		return fmt.Errorf("docker compose %s：%w", strings.Join(arguments, " "), err)
	}
	return nil
}

func composeProjectName(installDir string) string {
	name := filepath.Base(filepath.Clean(installDir))
	if name == "." || name == string(filepath.Separator) || name == "" {
		return "open-ai-canvas"
	}
	var builder strings.Builder
	for _, char := range strings.ToLower(name) {
		if (char >= 'a' && char <= 'z') || (char >= '0' && char <= '9') || char == '-' || char == '_' {
			builder.WriteRune(char)
		}
	}
	if result := strings.Trim(builder.String(), "-_ "); result != "" {
		return result
	}
	return "open-ai-canvas"
}

func (m *Manager) writeComposeEnvOverride(images deploymentImages) (string, error) {
	data, err := os.ReadFile(m.envPath())
	if err != nil {
		return "", err
	}
	path := filepath.Join(m.config.StateDir, ".compose-env-override-"+randomID())
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return "", err
	}
	cleanup := func() { _ = os.Remove(path) }
	if err := setEnvValue(path, "CANVAS_BACKEND_IMAGE", images.backend); err != nil {
		cleanup()
		return "", err
	}
	if err := setEnvValue(path, "CANVAS_WEB_IMAGE", images.web); err != nil {
		cleanup()
		return "", err
	}
	if err := setEnvValue(path, "CANVAS_YINGCE_AGENT_IMAGE", images.agent); err != nil {
		cleanup()
		return "", err
	}
	return path, nil
}

func (m *Manager) verifyImages(images deploymentImages) (deploymentImages, error) {
	refs := []string{images.backend, images.web, images.agent}
	digests := make([]string, 0, len(refs))
	for _, image := range refs {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		var output bytes.Buffer
		var stderr bytes.Buffer
		err := m.runner.Run(ctx, "docker", []string{"image", "inspect", image, "--format", "{{json .RepoDigests}}"}, nil, &output, &stderr)
		cancel()
		if err != nil {
			return deploymentImages{}, fmt.Errorf("校验目标镜像摘要失败：%s", strings.TrimSpace(stderr.String()))
		}
		var repositoryDigests []string
		if err := json.Unmarshal(output.Bytes(), &repositoryDigests); err != nil {
			return deploymentImages{}, fmt.Errorf("解析目标镜像摘要失败：%w", err)
		}
		repository := strings.Split(image, ":")[0]
		var digest string
		for _, candidate := range repositoryDigests {
			if strings.HasPrefix(candidate, repository+"@sha256:") {
				decoded, err := hex.DecodeString(strings.TrimPrefix(candidate, repository+"@sha256:"))
				if err != nil || len(decoded) != sha256.Size {
					continue
				}
				digest = candidate
				break
			}
		}
		if digest == "" {
			return deploymentImages{}, fmt.Errorf("目标镜像 %s 未包含仓库摘要", image)
		}
		digests = append(digests, digest)
	}
	return deploymentImages{backend: digests[0], web: digests[1], agent: digests[2]}, nil
}

func setDeploymentImages(path, version string, images deploymentImages) error {
	if err := setEnvValue(path, "CANVAS_IMAGE_TAG", strings.TrimPrefix(version, "v")); err != nil {
		return err
	}
	if err := setEnvValue(path, "CANVAS_BACKEND_IMAGE", images.backend); err != nil {
		return err
	}
	if err := setEnvValue(path, "CANVAS_WEB_IMAGE", images.web); err != nil {
		return err
	}
	if err := setEnvValue(path, "CANVAS_YINGCE_AGENT_IMAGE", images.agent); err != nil {
		return err
	}
	return ensureAgentToken(path)
}

func ensureAgentToken(path string) error {
	values, err := readEnvFile(path)
	if err != nil {
		return err
	}
	if len(strings.TrimSpace(values["YINGCE_AGENT_TOKEN"])) >= 32 {
		return nil
	}
	token := make([]byte, 32)
	if _, err := rand.Read(token); err != nil {
		return fmt.Errorf("生成 Agent 服务凭证：%w", err)
	}
	return setEnvValue(path, "YINGCE_AGENT_TOKEN", hex.EncodeToString(token))
}

func (m *Manager) createBackup(version string) (Backup, error) {
	now := time.Now().UTC()
	id := "backup-" + now.Format("20060102-150405")
	path := filepath.Join(m.config.BackupDir, id+".zip")
	temporary := path + ".partial"
	file, err := os.OpenFile(temporary, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return Backup{}, fmt.Errorf("创建备份文件：%w", err)
	}
	removeTemporary := true
	defer func() {
		_ = file.Close()
		if removeTemporary {
			_ = os.Remove(temporary)
		}
	}()
	hasher := sha256.New()
	archive := zip.NewWriter(io.MultiWriter(file, hasher))
	metadata, _ := json.MarshalIndent(map[string]any{"id": id, "version": version, "createdAt": now, "format": 1}, "", "  ")
	if err := writeZipBytes(archive, "metadata.json", metadata); err != nil {
		return Backup{}, err
	}
	databaseEntry, err := archive.CreateHeader(&zip.FileHeader{Name: "database.dump", Method: zip.Store})
	if err != nil {
		return Backup{}, err
	}
	if err := m.backupDatabase(version, databaseEntry); err != nil {
		return Backup{}, fmt.Errorf("备份 PostgreSQL：%w", err)
	}
	dataEntry, err := archive.CreateHeader(&zip.FileHeader{Name: "backend-data.tar", Method: zip.Store})
	if err != nil {
		return Backup{}, err
	}
	// Backend 已停止；复用旧镜像与原数据卷，只执行 tar，不启动业务或依赖服务。
	if err := m.compose(m.composePath(), version, m.config.StepTimeout, dataEntry, "run", "--rm", "--no-deps", "--pull", "never", "-T", "--user", "root", "--entrypoint", "tar", "backend", "-C", "/data", "-cf", "-", "."); err != nil {
		return Backup{}, fmt.Errorf("备份数据目录：%w", err)
	}
	if err := archive.Close(); err != nil {
		return Backup{}, fmt.Errorf("完成 ZIP 备份：%w", err)
	}
	if err := file.Sync(); err != nil {
		return Backup{}, fmt.Errorf("同步 ZIP 备份：%w", err)
	}
	if err := file.Close(); err != nil {
		return Backup{}, err
	}
	if err := os.Rename(temporary, path); err != nil {
		return Backup{}, fmt.Errorf("提交 ZIP 备份：%w", err)
	}
	removeTemporary = false
	stat, err := os.Stat(path)
	if err != nil {
		return Backup{}, err
	}
	checksum := "sha256:" + hex.EncodeToString(hasher.Sum(nil))
	if err := verifyZipBackup(path, checksum); err != nil {
		return Backup{}, err
	}
	return Backup{ID: id, Path: path, Checksum: checksum, Size: stat.Size(), CreatedAt: now, Version: version}, nil
}

func (m *Manager) backupDatabase(version string, output io.Writer) error {
	if m.config.DatabaseMode == DatabaseModeExternal {
		return m.compose(
			m.migrationComposePath(), version, m.config.StepTimeout, output,
			"run", "--rm", "--no-deps", "--pull", "never", "-T",
			"--entrypoint", "sh", "db-tools", "-ec",
			`exec pg_dump --dbname="$DATABASE_URL" --format=custom`,
		)
	}
	values, err := readEnvFile(m.envPath())
	if err != nil {
		return err
	}
	postgresUser := firstNonEmpty(values["POSTGRES_USER"], "open_ai_canvas")
	postgresDB := firstNonEmpty(values["POSTGRES_DB"], "open_ai_canvas")
	return m.compose(m.composePath(), version, m.config.StepTimeout, output, "exec", "-T", "postgres", "pg_dump", "-U", postgresUser, "-d", postgresDB, "-Fc")
}

func writeZipBytes(writer *zip.Writer, name string, data []byte) error {
	entry, err := writer.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Deflate})
	if err != nil {
		return err
	}
	_, err = entry.Write(data)
	return err
}

func verifyZipBackup(path, expected string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	hasher := sha256.New()
	if _, err := io.Copy(hasher, file); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	actual := "sha256:" + hex.EncodeToString(hasher.Sum(nil))
	if actual != expected {
		return fmt.Errorf("ZIP 备份校验失败：期望 %s，实际 %s", expected, actual)
	}
	archive, err := zip.OpenReader(path)
	if err != nil {
		return fmt.Errorf("ZIP 备份无法打开：%w", err)
	}
	defer archive.Close()
	required := map[string]bool{"metadata.json": false, "database.dump": false, "backend-data.tar": false}
	for _, entry := range archive.File {
		if _, ok := required[entry.Name]; ok && entry.UncompressedSize64 > 0 {
			required[entry.Name] = true
		}
	}
	for name, present := range required {
		if !present {
			return fmt.Errorf("ZIP 备份缺少有效文件 %s", name)
		}
	}
	return nil
}

func (m *Manager) restoreDatabase(backup Backup) error {
	if err := verifyZipBackup(backup.Path, backup.Checksum); err != nil {
		return fmt.Errorf("拒绝恢复未通过校验的备份：%w", err)
	}
	archive, err := zip.OpenReader(backup.Path)
	if err != nil {
		return err
	}
	defer archive.Close()
	var dump *zip.File
	for _, entry := range archive.File {
		if entry.Name == "database.dump" {
			dump = entry
			break
		}
	}
	if dump == nil {
		return errors.New("备份中不存在 database.dump")
	}
	reader, err := dump.Open()
	if err != nil {
		return err
	}
	defer reader.Close()
	if m.config.DatabaseMode == DatabaseModeExternal {
		return m.restoreExternalDatabase(backup.Version, reader)
	}
	return m.restoreManagedDatabase(backup.Version, reader)
}

func (m *Manager) restoreManagedDatabase(version string, input io.Reader) error {
	values, err := readEnvFile(m.envPath())
	if err != nil {
		return err
	}
	postgresUser := firstNonEmpty(values["POSTGRES_USER"], "open_ai_canvas")
	postgresDB := firstNonEmpty(values["POSTGRES_DB"], "open_ai_canvas")
	if postgresDB == "postgres" || strings.HasPrefix(postgresDB, "template") {
		return fmt.Errorf("拒绝覆盖 PostgreSQL 系统数据库 %q", postgresDB)
	}
	if err := m.compose(m.composePath(), version, 5*time.Minute, nil, "exec", "-T", "postgres", "dropdb", "-U", postgresUser, "--if-exists", "--force", postgresDB); err != nil {
		return fmt.Errorf("删除待恢复数据库：%w", err)
	}
	if err := m.compose(m.composePath(), version, 5*time.Minute, nil, "exec", "-T", "postgres", "createdb", "-U", postgresUser, "-O", postgresUser, postgresDB); err != nil {
		return fmt.Errorf("重建待恢复数据库：%w", err)
	}
	if err := m.composeWithInput(m.composePath(), version, m.config.StepTimeout, input, nil, "exec", "-T", "postgres", "pg_restore", "-U", postgresUser, "-d", postgresDB, "--no-owner", "--no-privileges"); err != nil {
		return fmt.Errorf("恢复 PostgreSQL：%w", err)
	}
	return nil
}

func (m *Manager) restoreExternalDatabase(version string, input io.Reader) error {
	if err := m.composeWithInput(
		m.migrationComposePath(), version, m.config.StepTimeout, input, nil,
		"run", "--rm", "--no-deps", "--pull", "never", "-T",
		"--entrypoint", "sh", "db-tools", "-ec",
		`exec pg_restore --clean --if-exists --no-owner --no-privileges --dbname="$DATABASE_URL"`,
	); err != nil {
		return fmt.Errorf("恢复外部 PostgreSQL：%w", err)
	}
	return nil
}

func (m *Manager) restoreBackendData(backup Backup) error {
	if err := verifyZipBackup(backup.Path, backup.Checksum); err != nil {
		return fmt.Errorf("拒绝恢复未通过校验的备份：%w", err)
	}
	archive, err := zip.OpenReader(backup.Path)
	if err != nil {
		return err
	}
	defer archive.Close()
	var data *zip.File
	for _, entry := range archive.File {
		if entry.Name == "backend-data.tar" {
			data = entry
			break
		}
	}
	if data == nil {
		return errors.New("备份中不存在 backend-data.tar")
	}
	reader, err := data.Open()
	if err != nil {
		return err
	}
	defer reader.Close()

	// Keep the /data mount itself intact; it may be a named volume or a host path.
	command := "find /data -mindepth 1 -maxdepth 1 -exec rm -rf -- {} + && tar -C /data -xf -"
	if err := m.composeWithInput(
		m.composePath(), backup.Version, m.config.StepTimeout, reader, nil,
		"run", "--rm", "--no-deps", "--pull", "never", "-T", "--user", "root",
		"--entrypoint", "sh", "backend", "-ec", command,
	); err != nil {
		return fmt.Errorf("恢复后端数据目录：%w", err)
	}
	return nil
}

func (m *Manager) verifyHealthy(targetVersion string) error {
	healthURL := m.healthURL()
	deadline := time.Now().Add(10 * time.Minute)
	stableSince := time.Time{}
	for time.Now().Before(deadline) {
		if err := m.checkHealthOnce(healthURL, targetVersion); err != nil {
			stableSince = time.Time{}
		} else if stableSince.IsZero() {
			stableSince = time.Now()
		} else if time.Since(stableSince) >= m.config.StableWindow {
			return nil
		}
		time.Sleep(2 * time.Second)
	}
	return fmt.Errorf("目标版本 %s 未在健康检查窗口内稳定就绪", targetVersion)
}

func (m *Manager) healthURL() string {
	if configured := strings.TrimSpace(m.config.HealthURL); configured != "" {
		return configured
	}
	values, err := readEnvFile(m.envPath())
	if err != nil {
		return "http://127.0.0.1:3000/api/health/ready"
	}
	return "http://127.0.0.1:" + firstNonEmpty(values["CANVAS_HTTP_PORT"], "3000") + "/api/health/ready"
}

func (m *Manager) checkHealthOnce(healthURL, targetVersion string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, healthURL, nil)
	response, err := m.httpClient.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("健康接口返回 HTTP %d", response.StatusCode)
	}
	var payload struct {
		Code int `json:"code"`
		Data struct {
			Build struct {
				Version string `json:"version"`
			} `json:"build"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 256<<10)).Decode(&payload); err != nil {
		return err
	}
	if payload.Code != 0 {
		return errors.New("健康接口业务状态异常")
	}
	if IsReleaseVersion(targetVersion) && payload.Data.Build.Version != "" && CompareVersions(payload.Data.Build.Version, targetVersion) != 0 {
		return fmt.Errorf("运行版本仍为 %s，期望 %s", payload.Data.Build.Version, targetVersion)
	}
	return nil
}

func readEnvFile(path string) (map[string]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取 %s：%w", path, err)
	}
	values := make(map[string]string)
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if ok {
			values[strings.TrimSpace(key)] = strings.Trim(strings.TrimSpace(value), "\"")
		}
	}
	return values, nil
}

func setEnvValue(path, key, value string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	found := false
	for index, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, key+"=") {
			lines[index] = key + "=" + value
			found = true
		}
	}
	if !found {
		lines = append(lines, key+"="+value)
	}
	stat, err := os.Stat(path)
	if err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".env-*.tmp")
	if err != nil {
		return err
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if err := temporary.Chmod(stat.Mode().Perm()); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.WriteString(strings.Join(lines, "\n") + "\n"); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryName, path)
}

func replaceFile(source, target string, mode os.FileMode) error {
	data, err := os.ReadFile(source)
	if err != nil {
		return err
	}
	if mode == 0 {
		if stat, statErr := os.Stat(target); statErr == nil {
			mode = stat.Mode().Perm()
		} else {
			mode = 0o600
		}
	}
	temporary, err := os.CreateTemp(filepath.Dir(target), ".compose-*.tmp")
	if err != nil {
		return err
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if err := temporary.Chmod(mode); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryName, target)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
