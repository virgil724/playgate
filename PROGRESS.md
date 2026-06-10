# PlayGate 任務進度看板

> 由管理代理維護。波次依依賴圖排定。

| Task | 內容 | 負責代理 | 波次 | 狀態 |
|------|------|----------|------|------|
| T1 | host 骨架、核心 interface/型別、graceful shutdown | agent-t1 (opus) | 1 | 🔄 進行中 |
| T7+T8 | CF Workers signaling + Realtime TURN credential | agent-t7t8 (sonnet) | 1 | 🔄 進行中 |
| T10 | playgate-server 最小 REST API | agent-t10 (sonnet) | 1 | 🔄 進行中 |
| T2+T3 | v4l2 擷取 + H.264 編碼 | - | 2 | ⏳ 等待 T1 |
| T4 | Pion WebRTC 模組 | - | 2 | ⏳ 等待 T1 |
| T5 | NXBT daemon + Go 橋接 | - | 2 | ⏳ 等待 T1 |
| T6 | 端到端串接 + 測試頁 | - | 3 | ⏳ 等待第 2 波 |
| T9 | Session 操控權管理 | - | 3 | ⏳ 等待 T1/T10 |
| T11+T12 | playgate-web 觀眾端 + 直播主後台 | - | 4 | ⏳ 等待 T10 |
| T13~T17 | 硬編碼/ABR/Sunshine/部署/合規 | - | 5 | ⏳ 等待 Phase 2 |

## 環境備註

- 開發機 Windows 11；host 目標 Linux（v4l2 / NXBT），以 `GOOS=linux go build` 交叉驗證
- Go 1.25.3、Node 22.18、無法在本機做硬體實測（擷取卡 / Switch / 藍牙），以 mock + 單元測試驗收
- Git 提交由管理代理在各波次完成後統一執行

## 已定案架構決策（給所有代理）

- 三專案 monorepo：`playgate-host/`（Go）、`playgate-web/`、`playgate-server/`（Go）、`playgate-signaling/`（CF Workers）
- Signaling = Cloudflare Workers；TURN = Cloudflare Realtime TURN；server 不自建這兩者
- Host：模組化 goroutine，channel pipeline，context 控生命週期，main 只接線
- Switch 控制：NXBT Python daemon + Unix socket 橋接
- 一條 WebRTC：MediaTrack 影音 + DataChannel（unreliable/unordered）指令
- CaptureSource / InputTarget 雙 interface 插件設計
