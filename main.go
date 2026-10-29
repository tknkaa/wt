// wt は git worktree の作成と、マージ済みブランチの片付けを行う。
//
//	wt new <branch> [<start>]  worktree を作り、.env をコピーして依存をインストールする
//	wt list                    worktree の一覧
//	wt clean [-y]              マージ済みの worktree とブランチを削除する
//	wt prune [-y]              clean に加え、close された PR やリモートが消えたブランチも削除する
package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const usage = `usage:
  wt new <branch> [<start>]  worktree を作り、.env をコピーして依存をインストールする
  wt list                    worktree の一覧
  wt clean [-y]              マージ済みの worktree とブランチを削除する
  wt prune [-y]              clean に加え、close された PR やリモートが消えたブランチも削除する

リポジトリ固有の後処理は git config wt.setup に書く(worktree 内で sh -c で実行する)。
  例: git config wt.setup "pnpm wrangler d1 migrations apply app --local"
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	var err error
	switch args := os.Args[2:]; os.Args[1] {
	case "new":
		if len(args) < 1 || len(args) > 2 {
			err = errors.New("usage: wt new <branch> [<start>]")
			break
		}
		start := ""
		if len(args) == 2 {
			start = args[1]
		}
		err = cmdNew(args[0], start)
	case "list", "ls":
		err = run("", "git", "worktree", "list")
	case "clean":
		err = cmdClean(len(args) > 0 && (args[0] == "-y" || args[0] == "--yes"), false)
	case "prune":
		err = cmdClean(len(args) > 0 && (args[0] == "-y" || args[0] == "--yes"), true)
	case "-h", "--help", "help":
		fmt.Print(usage)
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "wt:", err)
		os.Exit(1)
	}
}

// linked worktree 内から実行しても main の root を指すよう、common git dir から求める
func repoRoot() (root, common string, err error) {
	common, err = output("", "git", "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return "", "", err
	}
	return filepath.Dir(common), common, nil
}

// .gitignore はリポジトリで共有されるため触らない。clone ごとの .git/info/exclude に冪等に追記する
func ensureExcluded(common, pattern string) error {
	path := filepath.Join(common, "info", "exclude")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for _, line := range strings.Split(string(data), "\n") {
		if line == pattern {
			return nil
		}
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	if len(data) > 0 && !strings.HasSuffix(string(data), "\n") {
		pattern = "\n" + pattern
	}
	_, err = fmt.Fprintln(f, pattern)
	return err
}

func defaultBranch(root string) string {
	// origin/HEAD が無い clone もあるため、取れなければ main とみなす
	if ref, err := output(root, "git", "symbolic-ref", "--short", "refs/remotes/origin/HEAD"); err == nil {
		return strings.TrimPrefix(ref, "origin/")
	}
	return "main"
}

func cmdNew(branch, start string) error {
	root, common, err := repoRoot()
	if err != nil {
		return err
	}
	if err := ensureExcluded(common, "/.worktree/"); err != nil {
		return err
	}
	dir := filepath.Join(root, ".worktree", strings.ReplaceAll(branch, "/", "-"))

	if exists(root, "refs/heads/"+branch) {
		if start != "" {
			return fmt.Errorf("ブランチ %s は既にあるため <start> は指定できない", branch)
		}
		// 既に別の場所で checkout 済みなら git worktree add が拒否するので、そのまま任せる
		err = run(root, "git", "worktree", "add", dir, branch)
	} else {
		if start == "" {
			start = defaultBranch(root)
		}
		err = run(root, "git", "worktree", "add", "-b", branch, dir, start)
	}
	if err != nil {
		return err
	}

	if err := copyEnvFiles(root, dir); err != nil {
		return err
	}
	if err := installDeps(dir); err != nil {
		return err
	}
	if setup, _ := output(root, "git", "config", "--get", "wt.setup"); setup != "" {
		fmt.Println("==> wt.setup:", setup)
		if err := run(dir, "sh", "-c", setup); err != nil {
			return fmt.Errorf("wt.setup が失敗した: %w", err)
		}
	}
	fmt.Println(dir)
	return nil
}

// 秘密情報はどの worktree でも同じでよく、無いと起動できないため root からコピーする。
// 既にあるものは上書きしない。worktree 側で書き換えた値を消さないため
func copyEnvFiles(root, dir string) error {
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	for _, e := range entries {
		name := e.Name()
		if !e.Type().IsRegular() || !isEnvFile(name) {
			continue
		}
		// git で管理されているものは checkout で既に入っているので対象外
		if _, err := output(root, "git", "ls-files", "--error-unmatch", name); err == nil {
			continue
		}
		dst := filepath.Join(dir, name)
		if _, err := os.Stat(dst); err == nil {
			continue
		}
		if err := copyFile(filepath.Join(root, name), dst); err != nil {
			return err
		}
		fmt.Println("==> copied", name)
	}
	return nil
}

func isEnvFile(name string) bool {
	if strings.HasSuffix(name, ".example") || strings.HasSuffix(name, ".sample") {
		return false
	}
	return name == ".env" || strings.HasPrefix(name, ".env.") || name == ".dev.vars" || strings.HasPrefix(name, ".dev.vars.")
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, info.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// 依存はコピーせずインストールし直す。ブランチで依存が変わったときにずれないようにするため
func installDeps(dir string) error {
	candidates := []struct {
		lock string
		cmd  []string
	}{
		{"pnpm-lock.yaml", []string{"pnpm", "install", "--frozen-lockfile"}},
		{"bun.lock", []string{"bun", "install", "--frozen-lockfile"}},
		{"bun.lockb", []string{"bun", "install", "--frozen-lockfile"}},
		{"yarn.lock", []string{"yarn", "install", "--frozen-lockfile"}},
		{"package-lock.json", []string{"npm", "ci"}},
		{"uv.lock", []string{"uv", "sync"}},
	}
	for _, c := range candidates {
		if _, err := os.Stat(filepath.Join(dir, c.lock)); err != nil {
			continue
		}
		fmt.Println("==>", strings.Join(c.cmd, " "))
		return run(dir, c.cmd[0], c.cmd[1:]...)
	}
	return nil
}

type worktree struct {
	path   string
	branch string
}

func worktrees(root string) ([]worktree, error) {
	out, err := output(root, "git", "worktree", "list", "--porcelain")
	if err != nil {
		return nil, err
	}
	var list []worktree
	var cur worktree
	for _, line := range append(strings.Split(out, "\n"), "") {
		switch {
		case strings.HasPrefix(line, "worktree "):
			cur = worktree{path: strings.TrimPrefix(line, "worktree ")}
		case strings.HasPrefix(line, "branch "):
			cur.branch = strings.TrimPrefix(line, "branch refs/heads/")
		case line == "" && cur.path != "":
			list = append(list, cur)
			cur = worktree{}
		}
	}
	return list, nil
}

type candidate struct {
	branch string
	path   string // worktree が無ければ空
	reason string
}

// prune はマージされていないブランチも消すため、clean とは分けて明示的に選ばせる
func cmdClean(yes, prune bool) error {
	root, _, err := repoRoot()
	if err != nil {
		return err
	}
	if prune {
		// ディレクトリを手で消した worktree が残るとブランチが checkout 中扱いになり消せないため、先に登録を外す。
		// 消えるのは git の管理情報だけなので確認は挟まない
		if err := run(root, "git", "worktree", "prune", "--verbose"); err != nil {
			return err
		}
	}
	// リモートで消えたブランチの参照を残すと、マージ済みかどうかの判定がずれるため先に整理する
	if err := run(root, "git", "fetch", "--prune", "--quiet"); err != nil {
		return err
	}
	def := defaultBranch(root)

	wts, err := worktrees(root)
	if err != nil {
		return err
	}
	pathOf := map[string]string{}
	for i, w := range wts {
		// root の worktree は消せないので、そこで checkout しているブランチは対象外にする
		if i == 0 {
			pathOf[w.branch] = "-"
			continue
		}
		pathOf[w.branch] = w.path
	}

	prs, err := prHeads(root)
	if err != nil {
		return err
	}

	out, err := output(root, "git", "for-each-ref", "--format=%(refname:short) %(objectname) %(upstream:track)", "refs/heads")
	if err != nil {
		return err
	}
	var cands []candidate
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		name, sha, gone := f[0], f[1], strings.Contains(line, "[gone]")
		if name == def || pathOf[name] == "-" {
			continue
		}
		var reason string
		// squash マージでは main の履歴に元のコミットが残らないため、PR の先頭コミットと一致するかで見る。
		// 一致しなければマージ後に積んだコミットがあるので消さない
		pr, hasPR := prs[name]
		if hasPR && pr.merged && pr.oid == sha {
			reason = fmt.Sprintf("PR #%d merged", pr.number)
		} else if gone && ancestor(root, sha, "origin/"+def) {
			// 作ったばかりでコミットの無いブランチも main の祖先になるため、push 済みでリモートが消えたものに限る
			reason = "merged into origin/" + def
		} else if prune && hasPR && !pr.merged && pr.oid == sha {
			// close 後に積んだコミットは作業を再開したものとみなし、PR の先頭と一致するものだけ消す
			reason = fmt.Sprintf("PR #%d closed", pr.number)
		} else if prune && gone {
			reason = "remote gone"
		} else {
			continue
		}
		cands = append(cands, candidate{branch: name, path: pathOf[name], reason: reason})
	}

	if len(cands) == 0 {
		fmt.Println("削除できるブランチはない")
		return nil
	}

	var targets []candidate
	for _, c := range cands {
		// 未コミットの変更を消すと戻せないため、tracked な変更がある worktree は残す。
		// node_modules など未追跡のものは作り直せるので見ない
		if c.path != "" {
			if st, _ := output(c.path, "git", "status", "--porcelain", "--untracked-files=no"); st != "" {
				fmt.Printf("skip  %-40s 未コミットの変更がある (%s)\n", c.branch, c.path)
				continue
			}
		}
		targets = append(targets, c)
		where := "(no worktree)"
		if c.path != "" {
			where = c.path
		}
		fmt.Printf("      %-40s %-24s %s\n", c.branch, c.reason, where)
	}
	if len(targets) == 0 {
		return nil
	}
	if !yes && !confirm(fmt.Sprintf("%d 個の worktree とブランチを削除する?", len(targets))) {
		return nil
	}

	for _, c := range targets {
		if c.path != "" {
			if err := run(root, "git", "worktree", "remove", "--force", c.path); err != nil {
				return err
			}
		}
		// squash マージだと git からは未マージに見えて -d が拒否されるため -D を使う。上で PR の先頭と一致することを確かめている
		if err := run(root, "git", "branch", "-D", c.branch); err != nil {
			return err
		}
	}
	return run(root, "git", "worktree", "prune")
}

type pullRequest struct {
	number int
	oid    string
	merged bool
}

// closed には merged も含まれるため、一度の呼び出しで両方を得る
func prHeads(root string) (map[string]pullRequest, error) {
	if _, err := exec.LookPath("gh"); err != nil {
		fmt.Fprintln(os.Stderr, "wt: gh が無いため、squash マージされた PR は判定できない")
		return map[string]pullRequest{}, nil
	}
	out, err := output(root, "gh", "pr", "list", "--state", "closed", "--limit", "500", "--json", "number,headRefName,headRefOid,state")
	if err != nil {
		// GitHub 以外のリモートでも通常のマージ判定だけは使えるよう、失敗しても続ける
		fmt.Fprintln(os.Stderr, "wt: gh pr list が失敗したため、PR での判定はしない:", err)
		return map[string]pullRequest{}, nil
	}
	var prs []struct {
		Number      int    `json:"number"`
		HeadRefName string `json:"headRefName"`
		HeadRefOid  string `json:"headRefOid"`
		State       string `json:"state"`
	}
	if err := json.Unmarshal([]byte(out), &prs); err != nil {
		return nil, err
	}
	m := map[string]pullRequest{}
	for _, p := range prs {
		// 同じブランチ名で複数回 PR を出した場合は新しいもの(先に返る)を使う
		if _, ok := m[p.HeadRefName]; !ok {
			m[p.HeadRefName] = pullRequest{p.Number, p.HeadRefOid, p.State == "MERGED"}
		}
	}
	return m, nil
}

func ancestor(root, sha, ref string) bool {
	return exec.Command("git", "-C", root, "merge-base", "--is-ancestor", sha, ref).Run() == nil
}

func exists(root, ref string) bool {
	return exec.Command("git", "-C", root, "show-ref", "--verify", "--quiet", ref).Run() == nil
}

func confirm(msg string) bool {
	fmt.Printf("%s [y/N] ", msg)
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	ans := strings.ToLower(strings.TrimSpace(line))
	return ans == "y" || ans == "yes"
}

func run(dir, name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Stdout, cmd.Stderr, cmd.Stdin = os.Stdout, os.Stderr, os.Stdin
	return cmd.Run()
}

func output(dir, name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return "", fmt.Errorf("%s %s: %s", name, strings.Join(args, " "), msg)
		}
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}
