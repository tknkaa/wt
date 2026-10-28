# wt

git worktree を `.worktree/<branch>` に作り、マージ済みのものを片付ける CLI。

```sh
go install github.com/tknkaa/wt@latest
```

## 使い方

```sh
wt new feat/foo [<start>]  # .worktree/feat-foo に作る。start 省略時は origin の既定ブランチ
wt list
wt clean [-y]              # マージ済みの worktree とブランチを確認してから削除
```

`wt new` は worktree を作ったあとに次を行う。

- `/.worktree/` を `.git/info/exclude` に追記する(`.gitignore` は触らない)
- root にある未追跡の `.env` / `.env.*` / `.dev.vars` をコピーする(`.example` と `.sample` は除く。既にあれば上書きしない)
- lockfile を見て依存をインストールする(pnpm / bun / yarn / npm / uv)
- `git config wt.setup` があれば worktree 内で実行する

```sh
git config wt.setup "pnpm wrangler d1 migrations apply app --local"
```

`wt clean` が消すのは次のどちらかに当たるブランチ。

- マージ済み PR の先頭コミットとブランチの先頭が一致する(squash マージ向け。`gh` が要る)
- リモートのブランチが消えていて、`origin/<既定ブランチ>` に含まれる

tracked な未コミットの変更がある worktree は消さない。
