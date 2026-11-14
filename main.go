// gh wt は git worktree の作成と、マージ済みブランチの片付けを行う。
//
//	gh wt new <branch> [<start>]  worktree を作り、.env をコピーして依存をインストールする
//	gh wt list                    worktree の一覧
//	gh wt clean [-y] [--gone]     open ではない PR に紐づく worktree とブランチを削除する
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
  gh wt new <branch> [<start>]  worktree を作り、.env をコピーして依存をインストールする
  gh wt list                    worktree の一覧
  gh wt clean [-y] [--gone]     open ではない PR に紐づく worktree とブランチを削除する
                             --gone でリモートが消えただけのブランチも削除する

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
			err = errors.New("usage: gh wt new <branch> [<start>]")
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
		err = cleanWithArgs(args, false)
	case "prune":
		// 旧コマンドで実行しても動くようにしつつ、clean へ寄せるため移行を促す
		fmt.Fprintln(os.Stderr, "gh wt: prune は廃止予定。gh wt clean --gone を使う")
		err = cleanWithArgs(args, true)
	case "-h", "--help", "help":
		fmt.Print(usage)
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "gh wt:", err)
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

func cleanWithArgs(args []string, gone bool) error {
	yes := false
	for _, a := range args {
		switch a {
		case "-y", "--yes":
			yes = true
		case "--gone":
			gone = true
		default:
			return fmt.Errorf("unknown option: %s\nusage: gh wt clean [-y] [--gone]", a)
		}
	}
	return cmdClean(yes, gone)
}

// --gone はマージも close もされていないブランチまで消すため、明示したときだけ対象にする
func cmdClean(yes, gone bool) error {
	root, _, err := repoRoot()
	if err != nil {
		return err
	}
	// ディレクトリを手で消した worktree が残るとブランチが checkout 中扱いになり消せないため、先に登録を外す。
	// 消えるのは git の管理情報だけなので確認は挟まない
	if err := run(root, "git", "worktree", "prune", "--verbose"); err != nil {
		return err
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
		name, sha, remoteGone := f[0], f[1], strings.Contains(line, "[gone]")
		if name == def || pathOf[name] == "-" {
			continue
		}
		var reason string
		pr, hasPR := prs[name]
		if hasPR && pr.open {
			// 作業中のブランチなので、ほかの条件に当たっても消さない
			continue
		}
		if hasPR {
			// PR が閉じたらそのブランチの作業は終わったとみなし、先頭コミットが PR と違っても消す。
			// ただし PR に載っていないコミットは消えると戻せないため、一覧で分かるようにする
			state := "closed"
			if pr.merged {
				state = "merged"
			}
			reason = fmt.Sprintf("PR #%d %s", pr.number, state)
			if pr.oid != sha {
				reason += " (+未 PR のコミット)"
			}
		} else if remoteGone && ancestor(root, sha, "origin/"+def) {
			// 作ったばかりでコミットの無いブランチも main の祖先になるため、push 済みでリモートが消えたものに限る
			reason = "merged into origin/" + def
		} else if gone && remoteGone {
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
		// squash マージだと git からは未マージに見えて -d が拒否されるため -D を使う。対象は一覧で確認を取っている
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
	open   bool
}

// open な PR があるブランチを対象から外すため、closed だけでなくすべての状態を得る
func prHeads(root string) (map[string]pullRequest, error) {
	if _, err := exec.LookPath("gh"); err != nil {
		fmt.Fprintln(os.Stderr, "gh wt: gh が無いため、squash マージされた PR は判定できない")
		return map[string]pullRequest{}, nil
	}
	out, err := output(root, "gh", "pr", "list", "--state", "all", "--limit", "1000", "--json", "number,headRefName,headRefOid,state")
	if err != nil {
		// GitHub 以外のリモートでも通常のマージ判定だけは使えるよう、失敗しても続ける
		fmt.Fprintln(os.Stderr, "gh wt: gh pr list が失敗したため、PR での判定はしない:", err)
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
		pr := pullRequest{p.Number, p.HeadRefOid, p.State == "MERGED", p.State == "OPEN"}
		cur, ok := m[p.HeadRefName]
		// 同じブランチ名で複数回 PR を出した場合、1 つでも open なら作業中なので open を優先する。
		// それ以外は新しいもの(先に返る)を使う
		if !ok || (pr.open && !cur.open) {
			m[p.HeadRefName] = pr
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
