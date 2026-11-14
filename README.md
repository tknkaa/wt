# gh-wt

git worktree を `.worktree/<branch>` に作り、使い終わったものを片付ける gh extension。

```sh
gh extension install tknkaa/gh-wt
gh extension upgrade wt
```

## 使い方

```sh
gh wt new feat/foo [<start>]  # .worktree/feat-foo に作る。start 省略時は origin の既定ブランチ
gh wt list
gh wt clean [-y] [--gone]     # open ではない PR に紐づく worktree とブランチを確認してから削除
```

`gh wt new` は worktree を作ったあとに次を行う。

- `/.worktree/` を `.git/info/exclude` に追記する(`.gitignore` は触らない)
- root にある未追跡の `.env` / `.env.*` / `.dev.vars` をコピーする(`.example` と `.sample` は除く。既にあれば上書きしない)
- lockfile を見て依存をインストールする(pnpm / bun / yarn / npm / uv)
- `git config wt.setup` があれば worktree 内で実行する

```sh
git config wt.setup "pnpm wrangler d1 migrations apply app --local"
```

`gh wt clean` が消すのは次のいずれかに当たるブランチ。

- マージ済みか close された PR がある。PR に載っていないコミットがあっても消すので、一覧で `+未 PR のコミット` と出たものは確認する
- リモートのブランチが消えていて、`origin/<既定ブランチ>` に含まれる
- `--gone` を付けたときだけ、リモートのブランチが消えたもの(マージされていなくても消す)

open な PR があるブランチは、上のどれに当たっても消さない。tracked な未コミットの変更がある worktree も消さない。ディレクトリを手で消した worktree の登録は、確認なしで外す。

`gh wt prune` は `gh wt clean --gone` の旧名で、廃止予定。

## リリース

`v*` のタグを push すると、GitHub Actions が各 OS 向けのバイナリをリリースに載せる。
