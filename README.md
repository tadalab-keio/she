# she ── PC から研究室のサーバへコマンドを送る（Efficient Shell）

各自の Windows PC から、`ssh tadalab` のサーバへ、**コマンドを標準入力で渡して実行する**道具（2026-10-12）。
Claude Code などのエージェントが、少ない入力と短い時間でサーバを操作するためのもの。人が打つときのために Tab の補完もある。

## 使い方

コマンドは標準入力だけで渡す。引数は、名前付きセッションの名前か、管理オプション。

| Git Bash | PowerShell | 動き |
|---|---|---|
| `she <<'E'`<br>`コマンド`<br>`E` | `@'`<br>`コマンド`<br>`'@ \| she` | **名前なし**：毎回まっさらな bash（設定ファイルは読まない・標準入力は空）。並べて流せる。標準出力と標準エラーは別。終了コードはスクリプトのもの |
| `she 名前 <<'E' … E` | `@' … '@ \| she 名前` | **名前付き**：その名前の bash で実行。`cd`・変数・関数が次の呼び出しに残る（下の「名前付きセッション」） |
| `she 名前 <<< 'y'` | `'y' \| she 名前` | 実行中のコマンドが入力を待っていれば、その入力として送る。待っていなければスクリプトとして実行 |
| `she 名前`（それだけ） | 同じ | **打った文字を画面に出さずに 1 行読んで**、その名前で実行中のコマンドへ送る（パスワード用）。何も実行中でなければ送らない |
| `she -l` | 同じ | 名前付きセッションの一覧（名前・`running`／`idle`） |
| `she -start`／`she -stop` | 同じ | 接続を先に張る／閉じる（閉じても次の呼び出しで張り直す） |
| `she -setup ホスト` | 同じ | つなぐ先を `~/.ssh/config` の別のホストにする（既定 `tadalab`） |

- 接続（1 本の SSH）は最初の呼び出しで張り、使い回す。見張りはしない。切れていたら次の呼び出しで張り直す。**実行の途中で切れたら、勝手に再実行しない**（終了コード 255）。
- 1 回の呼び出しは約 0.09 秒（うち約 0.07 秒は Windows が `she.exe` を起動する時間）。最初の 1 回は接続を張るので約 0.8 秒。

### 名前付きセッション

名前ごとに、サーバに 1 本の**対話の bash（端末付き）**を持つ。`~/.bashrc`・エイリアス・再起動の表示が効き、`sudo`・`su`・`ssh` のパスワード欄も出る。

| 起きること | she の動き |
|---|---|
| コマンドが終わった | 終了コードを返し、最後にプロンプト（`[codex@tadalab tmp]$`）を**標準エラー**に出す（`$(...)` で受け取る中身は汚さない） |
| コマンドが入力を待っている | **終了コード 254** で返し、待っている理由を出す。続けて `she 名前 <<< '答え'` で入力を送る、`she 名前 </dev/null` で入力を送らずに続きを待つ、`she 名前 <<< $'\x03'` で Ctrl+C |
| 長く動くコマンド（出力なしの `sleep` など） | 終わるまで待つ（時間で切らない） |
| `exit` | その bash は終わる。次の呼び出しで新しく作る |

**入力待ちの見分け方**（時間ではなく、中の状態を見る）：

| 状態 | 判定 |
|---|---|
| 端末の echo が切れていて、行単位の入力のまま | パスワード欄（`su`・`getpass`・sudo 自身のパスワード欄） |
| 前面のプロセスが端末から `read` している | 入力待ち（`read`・`cat`・`python3` の対話など） |
| 中が見えないプロセス（`sudo` で動くもの・別のユーザーのもの）か、端末とほかを同時に待っているもの（`telnet` など） | 出力が改行で終わらないまま 1 秒止まったら、入力待ちとみなす（`Is this ok [y/N]: ` など） |

- `sudo` で動かしたコマンドは、`sudo` が自前の端末を張り、こちらの端末を raw にするので、中が見えない。改行で終わる出力のあとに入力を待つものは、知らせられない。
- 同じ名前への呼び出しは**順番に**動く。すぐ届くのは、制御文字 1 つ（Ctrl+C）と、`she 名前` 単体の見えない入力、サーバからの `she 名前` だけ。
- 削っている端末の機能：行の編集（readline）・履歴（`~/.bash_history` に書かない・`!` の展開なし）・色（`TERM=dumb`）・ページャ（`PAGER=cat`・`GIT_PAGER=cat`）。標準出力と標準エラーは 1 本にまとまる。画面全体を使うプログラム（`vim`・`top`）は使えない（キーを待つので 254 で返る）。

## エージェント（Claude Code・Codex）の使い方

- サーバのコマンドは `she <<'E'` 〜 `E` で送る（引用符付きのヒアドキュメントなので、中身は手元で展開されない）。`ssh tadalab 'コマンド'` を使うと、サーバが「Read ~/AGENTS.md … and use she instead of ssh.」と返す。
- `cd` や変数を続けて使うときだけ名前付き（`she w <<'E'`）。同じ名前を並べて呼ばない（順番待ちになる）。
- **254 が返ったら**、出力の最後の行（プロンプト）を読んで答える。**パスワードは打たない**：ユーザーに「`she 名前` と打って、パスワードを入れてください」と頼み、届いたら `she 名前 </dev/null` で続きを受け取る。

## 人が打つとき（Tab）

| 打つもの | Git Bash（とサーバの対話シェル） | PowerShell |
|---|---|---|
| `she <` → Tab | `she <<<'│'`（`│` はカーソル） | `'│' \| she` |
| もう一度 Tab／Shift+Tab | `she <<'E'` ／ `│` ／ `E` と行き来 | `@'` ／ `│` ／ `'@ \| she` と行き来 |
| `she 名前 <` → Tab | 名前を保ったまま同じ | 同じ |
| `she <<` → Tab | `she <<'E'` ／ `│` ／ `E` | ─ |
| 引用符の中・本文の行で Tab（`cd ./l` など） | サーバの bash（名前付きならその bash のディレクトリ）に候補を聞いて補完。複数なら readline の一覧 | 同じ。複数で共通部分も伸びないときは、Tab／Shift+Tab で候補を順に入れる |
| `she pw` → Tab | 名前付きセッションの名前 | 同じ |
| 本文の中の Enter | 文字のある行なら改行を足す。空の行なら（その行を消して）実行。`E` の行なら実行 | Shift+Enter で改行、Enter で実行（PSReadLine のまま） |

- ほかの Tab・Shift+Tab・Enter はふだんのまま。
- 行末で候補が 1 つのときは、readline が閉じ引用符を足し、カーソルはその後ろに来る（readline の決まり）。閉じ引用符を先に打っておけば、カーソルは引用符の前に残る。
- Tab の補完は、ふだん PowerShell を使うなら、PowerShell で `bash` と打てば同じ端末で Git Bash に入れる（`exit` で戻る。`Install-She.ps1` が入れる）。

## サーバの上から（`ssh tadalab` の中）

サーバの `she`（`~/bin/she` → [`she-server.py`](she-server.py)）は、PC の she と同じ形で使える。tmux の代わりに、PC で動いている名前付きの bash へ入力を送るためのもの。

| 打つもの | 動き |
|---|---|
| `she <<'E' … E` | サーバ自身のまっさらな bash で実行 |
| `she 名前 <<'E'`／`she 名前 <<< 'x'` | PC が持っている名前の bash で実行、または入力として送る |
| `she 名前`（それだけ） | 見えない 1 行を送る（パスワード） |
| `she -l` | 名前の一覧 |

名前付きの bash を作れるのは PC の she だけ（PC のデーモンが持っているため）。Tab は PC の Git Bash と同じ（`server-completion.bash` が `completion.bash` を読む。codex の `~/.bashrc` の末尾の 1 行で読む）。

## 入れ方

**PC**（Windows 10/11、Git for Windows。`ssh tadalab` が鍵で通ること ── [`~/lab-tools/lab-ssh/README.md`](../lab-ssh/README.md)）：

```powershell
scp tadalab:lab-tools/she/Install-She.ps1 $env:TEMP\
powershell -NoProfile -ExecutionPolicy Bypass -File $env:TEMP\Install-She.ps1
```

`she.exe` を `%USERPROFILE%\bin` に置き、そこをユーザーの PATH に足し、Tab のファイル（`%USERPROFILE%\.config\she\`）と、Git Bash の `~/.bashrc`・PowerShell のプロファイルに読む行を足す。済んでいる段は飛ばすので、**更新するときも同じものを流す**。開き直した端末から効く。

**サーバ**（codex のアカウント。済んでいる）：`~/bin/she` → `~/lab-tools/she/she-server.py`、`~/.bashrc` の末尾で `server-completion.bash` を読む。

**作り直す**：ソース（`main.go`・`modes.go`・`completion.bash`・`completion.ps1`）を直したら、サーバで `bash ~/lab-tools/she/build.sh`（検査とビルドをして、このフォルダの `she.exe` に置く。2026-10-12 までは Drive のバックアップを避けるつもりで `/usr/local/share/she/` に置いていたが、`/usr/local` もバックアップされていたので、ソースの横に戻した）。そのあと各 PC で `Install-She.ps1`。

## しくみ

```
she.exe（呼ぶたびに起動）
  └─ localhost の TCP ＋ 合言葉 → デーモン（she.exe -daemon。最初の呼び出しが窓なしで起動。
                                     状態は %USERPROFILE%\.she\state.json）
        └─ SSH 接続 1 本（Go の golang.org/x/crypto/ssh。行き先・鍵・known_hosts は ssh -G tadalab から）
              └─ 取り次ぎ役（サーバの python3。デーモンごとに 1 つ）
                    ├─ 名前なし：呼び出しごとに bash --norc --noprofile -c
                    ├─ 名前付き：名前ごとに、端末（pty）付きの bash -i
                    └─ ソケット /run/user/<uid>/she/<pid>.sock（サーバの she が使う。本人だけ開ける）
```

- **名前付きの終わりの印**：スクリプトはファイルに書いて `. ファイル` で読ませ、終わりは `PROMPT_COMMAND`（bash がプロンプトを出す直前に毎回実行する）から、`\0@@she-end-<合言葉> <呼び出しの番号> <終了コード> <プロンプト>` を出させて知る。Ctrl+C で行が打ち切られても印は出る。番号の違う古い印は捨てる。打ち込んだ 1 行のエコーは出力から外す。
- **デーモンの起動は 1 つずつ**：`%USERPROFILE%\.she\start.lock` を取った呼び出しだけが起動する（同時の最初の呼び出しで、デーモンが 2 つ立たない）。
- **状態ファイルを `AppData` に置かない理由**：Claude のデスクトップアプリ（MSIX）から起動したプログラムが `AppData` に書くものは、アプリ専用の置き場へ振り替えられ、利用者の端末からは見えない。置いていたころ、エージェントの she と利用者の she が別々のデーモンを立てた（2026-10-12）。
- **サーバの案内との関係**：she の接続は `SHE=1` を付けて始めるので、`~/.bashrc` の「use she instead of ssh」の案内は出ない。
- 取り次ぎ役の方式は 3 つ比べて決めた（`SHE_MODE=channel`／`bash`／既定の `agent`）：毎回 SSH のチャネルを開く `channel` は 0.24〜0.42 秒、bash を使い回す `bash` は 0.16〜0.46 秒、取り次ぎ役の `agent` は 0.15〜0.30 秒（2026-10-11、手元の起動込み）。

## 方式の比較（2026-10-12、sho11 の PC。`true` を 6 回と、`sleep 1` を 3 つ同時に）

| 方式 | 1 回（秒） | `sleep 1` ×3 を同時に | 必要なもの・弱いところ |
|---|---|---|---|
| ssh を毎回（Windows の OpenSSH） | 0.63〜0.74 | 1.50 | Windows の OpenSSH には接続を使い回す ControlMaster が無い |
| Plink を毎回 | 0.41〜0.98 | 1.60 | 鍵を PuTTY 形式（.ppk）に直す・ホスト鍵を別に渡す |
| Plink の接続共有（`-share`、上流を 1 本常駐） | 0.15〜0.39 | 1.25 | 上に加えて上流の常駐が要る。名前付きの bash は無い |
| rsh（FIFO で 1 本の bash に打ち込む） | 0.22〜0.37 | 並べられない | 1 本の bash を順番に使う |
| **she 名前なし** | **0.07〜0.11** | **1.13** | ─ |
| she 名前付き | 0.08〜0.33 | 順番に動く | ─ |

Plink は 0.84（`C:\Program Files\PuTTY\plink.exe`）。比べるために鍵を一時的に .ppk に直し、測り終えて消した。測った台本は `~/claude-work/she/`（このファイルの横にはない）。

## ファイル

| ファイル | 中身 |
|---|---|
| `main.go` | PC の she（呼び出し・デーモン・SSH） |
| `modes.go` | 取り次ぎ役の 3 方式。`agentCode` がサーバで動く Python（名前付きの bash・入力待ちの見分け・ソケット・補完の問い合わせ） |
| `completion.bash`／`completion.ps1` | Tab（`she.exe` に埋め込み、`she --bash-completion`／`--powershell-completion` で出す） |
| `she-server.py`・`server-completion.bash` | サーバの she と、その Tab |
| `build.sh`・`Install-She.ps1` | 作る・PC に入れる |
| `she.exe` | `build.sh` が作ったもの（`Install-She.ps1` がここから取る） |
| `go.mod`・`go.sum` | Go の部品（`golang.org/x/crypto`・`golang.org/x/term`） |

## 分かっている制限

- 名前付きの bash は、PC のデーモンが生きている間だけある（`she -stop`・PC の再起動・she の入れ替えで消える）。
- `sudo` の中で動くコマンドの入力待ちは、改行で終わらないまま 1 秒止まったときしか分からない。
- PowerShell 5.1 では `she -s` のように `-` で始まる語は Tab で補完されない（PowerShell が自分の引数として扱う）。
- Git Bash で `cat <<X` と Enter した後の `>` の行では、最初の語は補完されない（she を入れない bash でも同じ）。
- Git Bash の専用の窓（mintty）での見えない入力は、`completion.bash` の包み関数（bash の `read -s`）で読む。mintty では試していない。
