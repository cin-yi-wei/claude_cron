# 基底必須是 glibc：host 的 claude 2.1.223 與 claude-cron 都是動態連結
# glibc 2.43 的原生 ELF，alpine（musl）跑不動它們。
FROM ubuntu:26.04

# tmux：claude 需要 PTY。git：沙盒要 commit。ripgrep/jq/less/procps：
# Claude Code 的 Grep 工具與分頁需要。
# 刻意不裝 python3 / node / go —— 那正是 develop 用來改寫自己政策檔的東西。
# 代價是 develop 在這個共用映像裡跑不動 pytest / npm test / go test，
# 這是明確的功能倒退，解法是 per-agent 映像（Agent.Image，Task 7/12）。
RUN apt-get update && apt-get install -y --no-install-recommends \
      tmux git ca-certificates ripgrep jq less procps \
 && rm -rf /var/lib/apt/lists/*

# uid 1000 與 host 的 conray 對齊：bind 進來的 worktree 與 sandbox root
# 屬於 conray，容器內用同一個 uid 才寫得動。
# ubuntu:26.04 本身內建一個 uid 1000 的 "ubuntu" 使用者（26.04 起的預設行
# 為），直接 useradd -u 1000 會撞號（exit 4）。先刪掉它，再建我們自己的
# "agent"，維持同一個 uid，行為與註解原意不變。
RUN userdel -r ubuntu && useradd -u 1000 -m -s /bin/bash agent

USER 1000:1000
