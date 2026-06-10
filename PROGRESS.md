# PlayGate 任務進度看板

> 由管理代理維護。波次依依賴圖排定。

| Task | 內容 | 負責代理 | 波次 | 狀態 |
|------|------|----------|------|------|
| T1 | host 骨架、核心 interface/型別、graceful shutdown | agent-t1 (opus) | 1 | ✅ 完成（commit 7ef8e1c） |
| T7+T8 | CF Workers signaling + Realtime TURN credential | agent-t7t8 (sonnet) | 1 | ✅ 完成（commit 0b75c62，27/27 測試） |
| T10 | playgate-server 最小 REST API | agent-t10 (sonnet) | 1 | ✅ 完成（commit 706874a，JWT 契約定案） |
| T2+T3 | v4l2 擷取 + H.264 編碼 | agent-t2t3 + 補完A (opus) | 2 | ✅ 完成（go4vl 棄用改純 Go ioctl） |
| T4 | Pion WebRTC 模組 | agent-t4 (opus) | 2 | ✅ 完成（13-byte 線格式定案） |
| T5 | NXBT daemon + Go 橋接 | agent-t5 + 補完B | 2 | ✅ 完成（28 協議測試） |
| T6 | 端到端串接 + 測試頁 | agent-t6 (opus) + 管理者收尾 | 3 | ✅ 完成（synthetic dev 模式冒煙通過） |
| T9 | Session 操控權管理 | agent-t9 + 補完B | 2 | ✅ 完成（修 shutdown panic） |
| T11+T12 | playgate-web 觀眾端 + 直播主後台 | agent-web (opus) + 管理者修契約 | 3 | ✅ 完成（auth 改走 control channel、非 trickle answer） |
| T13+T14 | 硬編碼抽象 + 自適應位元率 | agent-abr (opus) | 4 | ✅ 完成（4 codec 矩陣、AIMD ABR） |
| T15 | Sunshine PC 模式 agent | agent-sunshine (sonnet) | 4 | ✅ 完成（14/14 測試） |
| T16 | Docker/安裝腳本/Makefile | agent-deploy (sonnet) | 4 | ✅ 完成（4 組交叉編譯實測） |
| T17 | 合規與法務文件 | agent-legal (sonnet) | 4 | ✅ 完成（README/TERMS/合規清單） |

## 全部 17 任務完成 ✅

剩餘需「實機」驗證的項目（無法在 Windows 開發機完成）：
- v4l2 struct 二進位佈局與真擷取卡（跑 `cmd/pipelinetest` 確認 .h264 可播）
- ffmpeg 真子程序互動、硬編碼各 codec 實際可用性
- NXBT 真藍牙配對與 Switch 實控
- 端到端延遲基準量測（README 延遲表待填）
- Docker build（本機無 daemon）、wrangler 正式部署（需 CF 帳號）
- Sunshine API 端點路徑需對照實際版本（已設計成可覆寫）

## 契約文件

`playgate-host/docs/protocols.md`：InputCommand 13-byte LE 線格式、按鍵 bitmask（18 鍵）、
NXBT socket NDJSON、control channel JSON、JWT claims（EdDSA）。前後端整合一律以此為準。

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
