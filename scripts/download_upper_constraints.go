// Скачивает пакеты из upper-constraints OpenStack (sdist, иначе wheel)
// и по желанию сразу заливает результат в GitHub-репозиторий.
//
// Логика скачивания:
//  1. Загрузить upper-constraints.txt (URL или локальный файл).
//  2. Убрать environment markers и дедуплицировать пины.
//  3. pip download --no-deps --no-binary :all:  (если не --skip-pip).
//  4. При ошибке — PyPI JSON API (sdist → wheel).
//
// Заливка на GitHub (--github-repo owner/name):
//  5. При --github-create создать репозиторий через gh (если нет).
//  6. git init / remote, add, commit, push.
//
// Примеры:
//
//	go run ./scripts/download_upper_constraints.go \
//	  --dest . \
//	  --packages-subdir packages/epoxy-2025.1 \
//	  --github-repo pvdro/Openstack \
//	  --skip-pip
//
//	go run download_upper_constraints.go \
//	  --dest ./mirror \
//	  --github-repo pvdro/Openstack \
//	  --github-create \
//	  --skip-pip
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// OpenStack Epoxy == 2025.1
const defaultConstraintsURL = "https://opendev.org/openstack/requirements/raw/branch/stable/2025.1/upper-constraints.txt"

const userAgent = "openstack-upper-constraints-downloader-go/1.1"

var pinRE = regexp.MustCompile(`^(.+?)={2,3}(.+)$`)

type pin struct {
	Name    string
	Version string
}

func (p pin) String() string {
	return p.Name + "===" + p.Version
}

type pypiFile struct {
	Filename    string `json:"filename"`
	URL         string `json:"url"`
	Packagetype string `json:"packagetype"`
}

type pypiRelease struct {
	URLs []pypiFile `json:"urls"`
}

func main() {
	urlFlag := flag.String("url", defaultConstraintsURL, "URL файла upper-constraints.txt (по умолчанию: Epoxy/2025.1)")
	constraintsFlag := flag.String("constraints", "", "Локальный upper-constraints.txt вместо скачивания")
	destFlag := flag.String("dest", "./upper-constraints-sources", "Корневой каталог результата")
	packagesSubdir := flag.String("packages-subdir", "src", "Подкаталог относительно --dest для архивов пакетов")
	pythonFlag := flag.String("python", "python3", "Интерпретатор Python для вызова pip")
	pipTimeout := flag.Duration("pip-timeout", 300*time.Second, "Таймаут pip download на один пакет")
	skipPip := flag.Bool("skip-pip", false, "Не использовать pip; скачивать всё только через PyPI JSON API")
	onlyPyPI := flag.Bool("only-pypi-fallback", false, "То же, что --skip-pip")
	continueMode := flag.Bool("continue", false, "Пропускать пины, для которых файл уже есть")

	// GitHub
	githubRepo := flag.String("github-repo", "", "Репозиторий GitHub owner/name — после скачивания commit+push")
	githubCreate := flag.Bool("github-create", false, "Создать репозиторий на GitHub, если его нет (нужен gh)")
	githubPrivate := flag.Bool("github-private", false, "Создавать приватный репозиторий")
	githubBranch := flag.String("branch", "main", "Ветка для push")
	commitMsg := flag.String("commit-message", "", "Сообщение коммита (по умолчанию генерируется)")
	noPush := flag.Bool("no-push", false, "Только commit, без push (имеет смысл с --github-repo)")
	githubDescription := flag.String("github-description", "OpenStack upper-constraints source mirror (Epoxy)", "Описание репозитория при создании")

	flag.Parse()

	usePip := !(*skipPip || *onlyPyPI)

	destRoot, err := filepath.Abs(*destFlag)
	must(err)
	srcDir := filepath.Join(destRoot, *packagesSubdir)
	must(os.MkdirAll(srcDir, 0o755))
	constraintsDir := filepath.Join(destRoot, "constraints")
	must(os.MkdirAll(constraintsDir, 0o755))

	client := &http.Client{Timeout: 10 * time.Minute}

	// --- 1. Загрузка constraints ---
	var text string
	if *constraintsFlag != "" {
		logf("Читаю constraints из %s", *constraintsFlag)
		b, err := os.ReadFile(*constraintsFlag)
		must(err)
		text = string(b)
		must(os.WriteFile(filepath.Join(constraintsDir, "upper-constraints.txt"), b, 0o644))
	} else {
		logf("Скачиваю constraints: %s", *urlFlag)
		b, err := httpGet(client, *urlFlag, 2*time.Minute)
		must(err)
		text = string(b)
		must(os.WriteFile(filepath.Join(constraintsDir, "upper-constraints.txt"), b, 0o644))
		// удобное имя для Epoxy-зеркала
		must(os.WriteFile(filepath.Join(constraintsDir, "upper-constraints-epoxy.txt"), b, 0o644))
	}

	pins := loadPinsFromConstraints(text)
	reqLines := make([]string, 0, len(pins))
	for _, p := range pins {
		reqLines = append(reqLines, p.String())
	}
	must(writeLines(filepath.Join(constraintsDir, "requirements-all.txt"), reqLines))
	must(writeLines(filepath.Join(constraintsDir, "requirements-all-epoxy.txt"), reqLines))

	logf("Уникальных пинов: %d", len(pins))
	logf("Каталог пакетов: %s", srcDir)
	if usePip {
		logf("Режим: pip (sdist), при ошибке — PyPI API")
	} else {
		logf("Режим: только PyPI API")
	}
	if *githubRepo != "" {
		logf("GitHub: после скачивания → %s (ветка %s)", *githubRepo, *githubBranch)
	}
	logf("")

	var (
		succeeded []string
		failed    []string
		viaPip    []string
		viaPyPI   []string
		skipped   []string
	)

	total := len(pins)
	for i, p := range pins {
		prefix := fmt.Sprintf("[%d/%d] %s", i+1, total, p.String())

		if *continueMode && alreadyHavePackage(srcDir, p.Name, p.Version) {
			logf("%s — пропуск (уже есть)", prefix)
			skipped = append(skipped, p.String())
			succeeded = append(succeeded, p.String())
			continue
		}

		ok := false
		detail := ""

		if usePip {
			logf("%s — pip download (sdist)...", prefix)
			if tryPipDownload(*pythonFlag, p, srcDir, *pipTimeout) {
				ok = true
				detail = "через pip"
				viaPip = append(viaPip, p.String())
			}
		}

		if !ok {
			if usePip {
				logf("%s — pip не смог, пробую PyPI API...", prefix)
			} else {
				logf("%s — PyPI API...", prefix)
			}
			ok, detail = tryPyPIDownload(client, p, srcDir)
			if ok {
				viaPyPI = append(viaPyPI, p.String()+" | "+detail)
				detail = "через PyPI (" + detail + ")"
			}
		}

		if ok {
			logf("%s — OK (%s)", prefix, detail)
			succeeded = append(succeeded, p.String())
		} else {
			logf("%s — ОШИБКА (%s)", prefix, detail)
			failed = append(failed, p.String()+" | "+detail)
		}
	}

	// --- отчёты ---
	must(writeLines(filepath.Join(destRoot, "succeeded.txt"), succeeded))
	must(writeLines(filepath.Join(destRoot, "failed.txt"), failed))
	must(writeLines(filepath.Join(destRoot, "via-pip.txt"), viaPip))
	must(writeLines(filepath.Join(destRoot, "via-pypi.txt"), viaPyPI))
	if len(skipped) > 0 {
		must(writeLines(filepath.Join(destRoot, "skipped-existing.txt"), skipped))
	}

	files, sdistN, wheelN, sizeMB := countArtifacts(srcDir)

	logf("")
	logf(strings.Repeat("=", 60))
	logf("СКАЧИВАНИЕ ЗАВЕРШЕНО")
	logf("  пинов:      %d", total)
	logf("  успешно:    %d", len(succeeded))
	logf("  ошибок:     %d", len(failed))
	logf("  через pip:  %d", len(viaPip))
	logf("  через PyPI: %d", len(viaPyPI))
	if len(skipped) > 0 {
		logf("  пропущено:  %d (уже были на диске)", len(skipped))
	}
	logf("  файлов:     %d  (sdist=%d, wheel=%d)", files, sdistN, wheelN)
	logf("  размер:     %.1f MB", sizeMB)
	logf("  каталог:    %s", srcDir)

	// --- GitHub ---
	if *githubRepo != "" {
		logf("")
		logf(strings.Repeat("=", 60))
		logf("ЗАЛИВКА НА GITHUB: %s", *githubRepo)
		msg := *commitMsg
		if msg == "" {
			msg = fmt.Sprintf("Update OpenStack upper-constraints packages (%d files, %.0f MB)", files, sizeMB)
		}
		if err := pushToGitHub(destRoot, *githubRepo, *githubBranch, msg, *githubCreate, *githubPrivate, *githubDescription, *noPush); err != nil {
			logf("ошибка GitHub: %v", err)
			os.Exit(1)
		}
		logf("GitHub: готово → https://github.com/%s", *githubRepo)
	}

	if len(failed) > 0 {
		logf("")
		logf("Не скачались:")
		for _, line := range failed {
			logf("  %s", line)
		}
		os.Exit(1)
	}
}

// ---------- GitHub / git ----------

func pushToGitHub(destRoot, repo, branch, commitMsg string, create, private bool, description string, noPush bool) error {
	if !commandExists("git") {
		return errors.New("git не найден в PATH")
	}
	if !commandExists("gh") {
		return errors.New("gh (GitHub CLI) не найден; установите: https://cli.github.com/ и выполните gh auth login")
	}

	// Проверка авторизации gh
	if out, err := exec.Command("gh", "auth", "status").CombinedOutput(); err != nil {
		return fmt.Errorf("gh не авторизован (%v): %s", err, strings.TrimSpace(string(out)))
	}

	if create {
		if err := ensureGitHubRepo(repo, private, description); err != nil {
			return err
		}
	} else {
		// убедиться, что репо существует
		if err := exec.Command("gh", "repo", "view", repo).Run(); err != nil {
			return fmt.Errorf("репозиторий %s не найден; укажите --github-create или создайте его вручную", repo)
		}
	}

	// .gitignore для служебного мусора
	gitignore := filepath.Join(destRoot, ".gitignore")
	if _, err := os.Stat(gitignore); errors.Is(err, os.ErrNotExist) {
		_ = os.WriteFile(gitignore, []byte("*.log\n__pycache__/\n.DS_Store\n/download_upper_constraints\n"), 0o644)
	}

	gitDir := filepath.Join(destRoot, ".git")
	if _, err := os.Stat(gitDir); errors.Is(err, os.ErrNotExist) {
		if err := runGit(destRoot, "init", "-b", branch); err != nil {
			return err
		}
	}

	// remote origin
	remoteURL := "https://github.com/" + repo + ".git"
	if out, err := exec.Command("git", "-C", destRoot, "remote", "get-url", "origin").CombinedOutput(); err != nil {
		if err := runGit(destRoot, "remote", "add", "origin", remoteURL); err != nil {
			return err
		}
	} else {
		cur := strings.TrimSpace(string(out))
		if cur != remoteURL && !strings.Contains(cur, repo) {
			logf("  remote origin уже указывает на %s — оставляю", cur)
		} else if cur != remoteURL {
			_ = runGit(destRoot, "remote", "set-url", "origin", remoteURL)
		}
	}

	// буфер для крупных push
	_ = runGit(destRoot, "config", "http.postBuffer", "524288000")

	if err := runGit(destRoot, "add", "-A"); err != nil {
		return err
	}

	// есть ли изменения?
	status, err := exec.Command("git", "-C", destRoot, "status", "--porcelain").Output()
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(status)) == "" {
		logf("  нет изменений для коммита")
		if noPush {
			return nil
		}
		// всё равно попробуем push (на случай пустого remote)
	} else {
		// user.email / user.name — если не заданы глобально, локальные заглушки
		ensureGitIdentity(destRoot)
		if err := runGit(destRoot, "commit", "-m", commitMsg); err != nil {
			return fmt.Errorf("git commit: %w", err)
		}
		logf("  commit: %s", commitMsg)
	}

	if noPush {
		logf("  --no-push: push пропущен")
		return nil
	}

	logf("  push origin %s ...", branch)
	// -u и force-with-lease не используем; обычный push
	cmd := exec.Command("git", "-C", destRoot, "push", "-u", "origin", branch)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		// если ветка разошлась с пустым remote history — попробовать main/master
		logf("  повтор push...")
		cmd2 := exec.Command("git", "-C", destRoot, "push", "-u", "origin", "HEAD:"+branch)
		cmd2.Stdout = os.Stdout
		cmd2.Stderr = os.Stderr
		if err2 := cmd2.Run(); err2 != nil {
			return fmt.Errorf("git push: %w", err2)
		}
	}
	return nil
}

func ensureGitHubRepo(repo string, private bool, description string) error {
	// уже есть?
	if exec.Command("gh", "repo", "view", repo).Run() == nil {
		logf("  репозиторий %s уже существует", repo)
		return nil
	}
	logf("  создаю репозиторий %s ...", repo)
	args := []string{"repo", "create", repo, "--description", description, "--confirm"}
	if private {
		args = append(args, "--private")
	} else {
		args = append(args, "--public")
	}
	cmd := exec.Command("gh", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func ensureGitIdentity(dir string) {
	if exec.Command("git", "-C", dir, "config", "user.email").Run() != nil {
		// попробовать глобальный; если нет — взять из gh
		if exec.Command("git", "config", "--global", "user.email").Run() != nil {
			login := "user"
			if out, err := exec.Command("gh", "api", "user", "--jq", ".login").Output(); err == nil {
				login = strings.TrimSpace(string(out))
			}
			_ = runGit(dir, "config", "user.email", login+"@users.noreply.github.com")
			_ = runGit(dir, "config", "user.name", login)
		}
	}
}

func runGit(dir string, args ...string) error {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git %s: %v\n%s", strings.Join(args, " "), err, string(out))
	}
	return nil
}

func commandExists(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

// ---------- скачивание (как раньше) ----------

func loadPinsFromConstraints(text string) []pin {
	seen := make(map[string]struct{})
	var pins []pin
	sc := bufio.NewScanner(strings.NewReader(text))
	buf := make([]byte, 0, 64*1024)
	sc.Buffer(buf, 1024*1024)

	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if i := strings.IndexByte(line, ';'); i >= 0 {
			line = strings.TrimSpace(line[:i])
		}
		p, ok := parsePin(line)
		if !ok {
			continue
		}
		key := p.String()
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		pins = append(pins, p)
	}
	return pins
}

func parsePin(line string) (pin, bool) {
	m := pinRE.FindStringSubmatch(strings.TrimSpace(line))
	if m == nil {
		return pin{}, false
	}
	return pin{Name: strings.TrimSpace(m[1]), Version: strings.TrimSpace(m[2])}, true
}

func tryPipDownload(python string, p pin, dest string, timeout time.Duration) bool {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, python, "-m", "pip", "download",
		"--no-deps", "--no-binary", ":all:", "-d", dest, p.String())
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	return cmd.Run() == nil
}

func tryPyPIDownload(client *http.Client, p pin, dest string) (bool, string) {
	data, err := pypiJSONFor(client, p.Name, p.Version)
	if err != nil {
		return false, "пакет не найден на PyPI: " + err.Error()
	}
	info, kind, ok := chooseArtifact(data.URLs)
	if !ok {
		return false, "на PyPI нет sdist/wheel"
	}
	out := filepath.Join(dest, info.Filename)
	if st, err := os.Stat(out); err == nil && st.Size() > 0 {
		return true, fmt.Sprintf("%s уже есть: %s", kind, info.Filename)
	}
	if err := downloadToFile(client, info.URL, out, 10*time.Minute); err != nil {
		_ = os.Remove(out)
		return false, "ошибка скачивания: " + err.Error()
	}
	st, _ := os.Stat(out)
	size := int64(0)
	if st != nil {
		size = st.Size()
	}
	return true, fmt.Sprintf("%s: %s (%d байт)", kind, info.Filename, size)
}

func pypiJSONFor(client *http.Client, name, version string) (*pypiRelease, error) {
	candidates := []string{name, strings.ReplaceAll(name, "_", "-")}
	tried := make(map[string]struct{})
	var lastErr error
	for _, n := range candidates {
		if _, ok := tried[n]; ok {
			continue
		}
		tried[n] = struct{}{}
		url := fmt.Sprintf("https://pypi.org/pypi/%s/%s/json", n, version)
		b, err := httpGet(client, url, time.Minute)
		if err != nil {
			lastErr = err
			continue
		}
		var rel pypiRelease
		if err := json.Unmarshal(b, &rel); err != nil {
			lastErr = err
			continue
		}
		return &rel, nil
	}
	if lastErr == nil {
		lastErr = errors.New("404")
	}
	return nil, lastErr
}

func chooseArtifact(urls []pypiFile) (pypiFile, string, bool) {
	var sdists, wheels []pypiFile
	for _, u := range urls {
		switch u.Packagetype {
		case "sdist":
			sdists = append(sdists, u)
		case "bdist_wheel":
			wheels = append(wheels, u)
		}
	}
	if len(sdists) > 0 {
		return sdists[0], "sdist", true
	}
	if len(wheels) == 0 {
		return pypiFile{}, "", false
	}
	for _, w := range wheels {
		fn := w.Filename
		if strings.Contains(fn, "py3-none-any") || strings.Contains(fn, "py2.py3-none-any") {
			return w, "wheel", true
		}
	}
	return wheels[0], "wheel", true
}

func alreadyHavePackage(dest, name, version string) bool {
	entries, err := os.ReadDir(dest)
	if err != nil {
		return false
	}
	nname := norm(name)
	nver := norm(version)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		fn := e.Name()
		nf := norm(fn)
		if (strings.HasPrefix(nf, nname+"-") || strings.HasPrefix(nf, nname+"_")) &&
			(strings.Contains(fn, version) || strings.Contains(nf, nver)) {
			return true
		}
	}
	return false
}

func norm(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	prevDash := false
	for _, r := range s {
		if r == '-' || r == '_' || r == '.' {
			if !prevDash {
				b.WriteByte('-')
				prevDash = true
			}
			continue
		}
		b.WriteRune(r)
		prevDash = false
	}
	return b.String()
}

func httpGet(client *http.Client, url string, timeout time.Duration) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, resp.Body) //nolint:errcheck
		return nil, fmt.Errorf("HTTP %d для %s", resp.StatusCode, url)
	}
	return io.ReadAll(resp.Body)
}

func downloadToFile(client *http.Client, url, dest string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, resp.Body) //nolint:errcheck
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	f, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(f, resp.Body)
	return err
}

func writeLines(path string, lines []string) error {
	var b strings.Builder
	for _, line := range lines {
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return os.WriteFile(path, []byte(b.String()), 0o644)
}

func countArtifacts(dir string) (files, sdistN, wheelN int, sizeMB float64) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, 0, 0, 0
	}
	var total int64
	for _, e := range entries {
		if e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		files++
		name := e.Name()
		switch {
		case strings.HasSuffix(name, ".whl"):
			wheelN++
		case strings.HasSuffix(name, ".tar.gz"),
			strings.HasSuffix(name, ".zip"),
			strings.HasSuffix(name, ".tar.bz2"),
			strings.HasSuffix(name, ".tgz"):
			sdistN++
		}
		if info, err := e.Info(); err == nil {
			total += info.Size()
		}
	}
	sizeMB = float64(total) / (1024 * 1024)
	return
}

func logf(format string, args ...any) {
	fmt.Printf(format+"\n", args...)
}

func must(err error) {
	if err != nil {
		fmt.Fprintf(os.Stderr, "ошибка: %v\n", err)
		os.Exit(1)
	}
}
