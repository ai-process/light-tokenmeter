# Agent Guidelines & Rules for light-tokenmeter

Guidelines and invariants for AI agents working in this codebase.

## 1. Centralized Issue Tracking (Правило ведения задач)
- **Все задачи для ВСЕХ репозиториев создаются ТОЛЬКО в приватном репозитории `docs` (`ai-process/docs`) в разделе Issues.**
- **All task discussions, planning, bug tracking, and feature requests take place exclusively in the Issues of the private repository `ai-process/docs`.** Do not open issues in this repository.

## 2. Code & Verification
- Keep comments to 3 lines maximum explaining *why*, never *what*.
- Concise messages: keep explanations short and direct.
- No AI attribution: never add `Co-Authored-By` trailers or AI attribution lines to git commits or PR descriptions.
- Account separation: never post PR comments using `gh` under a user's personal account credentials.
- Verification: run `go test ./...` before committing.
