# 端到端延遲量測紀錄

量的是「觀眾按鍵 → 畫面上看到這個按鍵」的時間（input-to-photon，不含螢幕掃描）。

## 方法

- **Host**：`capture.source: testsrc` 加 `input.target: overlay`。ffmpeg 把收到的按鍵（`BTN  A`）用黃字疊在畫面上。
- **觀眾**：Android 手機（Pixel）上的 Chrome。電腦用 `adb forward tcp:9333 localabstract:chrome_devtools_remote`，透過 CDP 在頁面裡執行 `scripts/latency/measure-btn.js`：
  ```sh
  node scripts/latency/cdp-eval.mjs <targetId> scripts/latency/measure-btn.js
  ```
  `targetId` 從 `http://127.0.0.1:9333/json/list` 取得。
- 腳本用頁面原本的鍵盤路徑送出 `KeyJ`（A 鍵），每一個 rAF 都比對畫面上 `BTN` 後面那一段的黃色像素，變化超過門檻就記下時間。按下和放開各算一筆，25 回合共 50 筆。兩個時間戳都用手機自己的時鐘，不需要對時。
- 頁面必須能操控：要嘛兌換 token，要嘛 host 設 `session.enabled: false`（直通模式，頁面會收到 `open` 事件）。

### 已知偏差

- overlay 每 25ms 才寫一次檔（`internal/input/overlay/target.go` 的 `flushInterval`），平均多算約 12ms，只有測試路徑有。
- 在電腦上單獨量「寫入按鍵 → ffmpeg 輸出畫面變化」：60fps 時 p50 35ms、max 59ms，已包含上面那 12ms。用實體擷取卡時，這一段會換成擷取卡本身的延遲。
- 手機上的 Chrome 鎖 60Hz rAF，偵測精度約 ±8ms。

### 量測陷阱（2026-11-18 踩過）

舊版腳本擷取整行 `BTN  -`，門檻設成「校正雜訊 × 3」。`- ↔ A` 只改一個字，像素差約 70。60fps 那輪背景雜訊較高，門檻被抬到 111，真正的變化抓不到，要等背景剛好擾動過門檻才觸發，量出 p50 635ms、max 2634ms 的假數據。
現在的版本只看會變的那一段，並回報 `noiseMax`、`threshold` 和 `signalMin`。**`signalMin` 必須明顯大於 `threshold`，結果才可信。**

## 結果

| 日期 | 網路 | 傳輸 | 編碼 | fps | n | p50 | p90 | 範圍 | 可信度 |
|---|---|---|---|---|---|---|---|---|---|
| 2026-11-18 | Wi-Fi（區網） | WebCodecs | CLI libx264 | 30 | 50 | 163 | 190 | | 尚可（門檻 48，訊號約 70，偏緊） |
| 2026-11-18 | Wi-Fi（區網） | RTP track | CLI libx264 | 30 | 50 | 198 | 228 | | 尚可（門檻 63，訊號約 70，偏緊） |
| 2026-11-18 | Wi-Fi | WebCodecs | libav libx264 | 60 | 50 | ~~635~~ | ~~2007~~ | 95–2634 | **無效**（門檻 111 > 訊號） |
| 2026-11-19 | 4G LTE（RTT 約 25～35ms） | WebCodecs | libav libx264 | 60 | 50 | **93** | 120 | 57–163 | 好（雜訊 1，門檻 20，訊號 90～102） |

單位 ms。2026-11-18 兩筆可信的資料用的是舊版腳本，門檻離訊號很近，數字可能略高。

### 這次 93ms 的粗估拆解（4G、60fps）

| 段落 | 約略 |
|---|---|
| Host 測試路徑（overlay 寫檔、等下一張畫面、ffmpeg 疊字） | 35ms |
| 編碼（libav） | 1ms |
| 網路來回（輸入上行加影像下行） | 25～35ms |
| 手機解碼（WebCodecs，硬體） | 17ms |
| 上畫面（60Hz） | 8～16ms |

### 還不能下的結論

新舊結果之間，網路、fps、編碼後端、腳本版本都不同，所以 163 → 93 不能直接算成優化幅度。要證明，必須在同一個網路上、用同一版腳本輪流量這幾種組合：

1. CLI、30fps、RTP（最舊的路徑）
2. CLI、30fps、WebCodecs
3. libav、60fps、WebCodecs（目前的路徑）
