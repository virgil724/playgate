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

### 同條件比較（2026-11-19 00:24–00:31，4G LTE，同一支手機、同一版腳本，間隔 10 分鐘內輪流量）

| 路徑 | 傳輸 | 編碼 | fps | n | 逾時 | p50 | p90 | max | 校正（雜訊／門檻／最小訊號） |
|---|---|---|---|---|---|---|---|---|---|
| 1. 最舊 | RTP track | CLI | 30 | 50 | 0 | 154 | 173 | 413 | 0 / 20 / 95 |
| 2. | WebCodecs | CLI | 30 | 50 | 0 | 129 | 154 | 157 | 2 / 20 / 91 |
| 3. 目前 | WebCodecs | libav | 60 | 50 | 0 | **91** | 112 | 139 | 3 / 20 / 25* |

\* 組合 3 有一筆偵測只有 25 個像素差（最小值 24ms），應該是雜訊誤觸發，對 p50 影響可忽略。00:17 另一輪同組合量到 p50 93、p90 120，兩輪一致。

**結論**（同網路、同腳本）：
- RTP → WebCodecs：p50 −25ms（154 → 129），長尾也收斂（max 413 → 157）。
- CLI 30fps → libav 60fps：再 −38ms（129 → 91）。主要來自 60fps：擷取等待和每張畫面的間隔都減半。這一步沒有拆開 backend 和 fps 各自的貢獻。
- 總計：p50 154 → 91（約 −41%），p90 173 → 112。
- CLI 後端收到補關鍵影格請求時會重啟 ffmpeg，畫面會停一下（量測期間共 8 次）；試跑時，這個停頓曾讓按鍵偵測逾時。libav 不需要重啟。

## 掉包與限速（2026-11-19 01:00–01:18，4G LTE，Android Chrome）

### 方法

- 用 [clumsy 0.3](https://github.com/jagt/clumsy)（WinDivert）在 host 電腦上動手腳，filter 是 `outbound and udp and udp.DstPort != 53`，只影響 host 送出的 UDP。
- 觀眾頁執行凍結偵測：監看 testsrc 左上角的 `frame N` 白字，每一張新畫面都會改變它。超過 150ms 沒變就算一次凍結。每組 60 秒。
- 一律 60fps。CLI 那兩組關閉 ABR（舊流程本來就沒有接 ABR），libav 那兩組開啟 ABR。

### 3% 隨機掉包

| 組合 | 傳輸 | 凍結次數 | 累計凍結 | 最長 | 畫面更新/秒 |
|---|---|---|---|---|---|
| A. CLI | RTP | 3 | 2.6 s | 1368 ms | 43.5 |
| B. CLI + intra-refresh | RTP | — | 60 s（全黑或全凍） | — | 0 |
| C. libav + PLI | RTP | 6 | 1.0 s | 183 ms | 46.0 |
| D. libav + FEC rs 0.2 | WebCodecs | 47 | 14.7 s | 767 ms | 31.2 |

- **C 最好**：RTP 的 NACK 重傳補回大部分掉包，補不回來時 PLI 會立刻補一張 IDR，最長凍結 183ms。
- **A**：凍結次數少，但每次都要等下一個 GOP（約 1～1.4 秒）才恢復。
- **B 不可用**：
  - 一開始完全黑畫面。這是 bug：觀眾加入時補的 IDR 在連線完成前就送出並被丟棄，已在 commit `74876c2` 修正。
  - 修正後能解出第一張，但只要一次掉包沒補回就永久凍結。Chrome 的 WebRTC 接收端畫面接不上時，會停下來等真正的關鍵影格，不接受 intra-refresh 的滾動恢復；CLI 後端又不回應 PLI。結論：**intra-refresh 對瀏覽器觀眾沒有用**。
- **D 很差，待查**：3% 封包掉落時，host 收到的分片遺失率回報是 7～10%（一個 SCTP 封包可能裝多個分片），20% 的 RS 同位分片看起來沒有發揮作用。ABR 也因此降到下限 1 Mbps。WebCodecs 傳輸在有掉包的網路上目前明顯輸給 RTP。

### 限速 150 KB/s（約 1.2 Mbps），libav + GCC，RTP

- GCC 有反應：限速後約 14 秒，碼率從 3.0 Mbps 降到 1.2 Mbps，之後停在 1.0 Mbps。
- 但 GCC 的估計值直接掉到下限 100 kbps（`gcc_target=100000`），明顯反應過度。編碼器卻被 `abr.min_bitrate`（1 Mbps）擋住，平滑處理也讓下降很慢。
- 結果是碼率一直貼著上限：前 60 秒凍結 72 次、累計 36 秒；之後 30 秒仍凍結 35 次、累計 12.8 秒。
- 待改：
  - 查 GCC 為什麼掉到地板（可能是 clumsy 的 bandwidth 模組直接丟封包，而不是排隊）；
  - GCC 模式下 `min_bitrate` 的預設值；
  - 下降時不要平滑處理，要立刻降。

### avcC fallback

在手機頁面載入前，用 CDP 注入腳本，讓 `VideoDecoder.configure` 拒絕沒有 `description` 的設定（模擬不支援 Annex-B 的瀏覽器）。網頁記錄到 `annex-b rejected; switching to avcC mode`，改用 `mode=avcc` 後正常解碼（45 fps、lost 0）。

## RTP 在 60fps 的解碼瓶頸（2026-11-19 01:30，4G LTE，Android Chrome）

libav、60fps、RTP 的延遲 p50 是 **241～257ms**，比 CLI、30fps、RTP 還慢。從 Chrome 接收端的統計（`inbound-rtp`）看，網路這段沒有問題：沒有 NACK、沒有 PLI、沒有掉包，jitter buffer 約 20ms。差別在解碼時間：

| 編碼 | fps | 每張解碼（`totalDecodeTime / framesDecoded`） |
|---|---|---|
| CLI | 30 | 17.5 ms |
| libav | 30 | 16.7 ms |
| CLI | 60 | 156 ms |
| libav | 60 | 132 ms |

這支手機的 Chrome 在 RTP 路徑上解一張約 17ms，看起來是軟體解碼器，一秒最多只能解約 58 張。60fps 時解碼器跟不上，畫面在佇列裡越排越長。WebCodecs 用的是硬體解碼器（約 17ms 是硬體的固定管線延遲，不會隨 fps 累積），所以 60fps 沒有這個問題。

| 路徑 | p50 | p90 | 範圍 |
|---|---|---|---|
| libav、30fps、RTP | **126** | 157 | 106–190 |
| libav、60fps、RTP | 241～257 | 290 | — |
| libav、60fps、WebCodecs | 91 | 112 | — |

**結論**：RTP 路徑要搭 30fps，不要用 60fps。

## WebCodecs 分片重傳（FRAG_NACK，2026-11-19 01:50–02:20，4G LTE）

前面「3% 隨機掉包」的組合 D 很差。網頁加上遺失原因統計後，查出原因：

- 3% 封包掉落時，分片遺失約 6%（為什麼是兩倍還沒查清楚）。
- 1 Mbps、60fps 下，大多數 delta 畫面只有 2 個分片（k=2），20% 的比例只配 1 個同位分片（m=1）。同一張畫面掉 2 個分片就救不回來，約 3% 的畫面會這樣，每一張都要整張重新請求 IDR。
- 沒有人為掉包時，也有畫面的分片超過 33ms 才到，被判定遺失。

修正：觀眾向 host 要求重傳缺少的分片（協定 4.5 節 `FRAG_NACK`），等待時間改成依 RTT 調整。

| 3% 掉包，60 秒 | 凍結次數 | 累計凍結 | 最長 |
|---|---|---|---|
| WebCodecs、只有 FEC（修正前） | 47 | 14.7 s | 767 ms |
| **WebCodecs、FEC + FRAG_NACK** | **3** | **0.66 s** | **252 ms** |
| 參考：libav + RTP（NACK + PLI） | 6 | 1.0 s | 183 ms |

乾淨網路（4G、4 Mbps，同一時段先後量；testsrc 的文字底框已改成不透明，避免背景干擾偵測）：

| | p50 | p90 | max |
|---|---|---|---|
| 修正前的網頁 | 97 | 130 | 307 |
| 修正後的網頁 | 97 | 114 | 124 |

延遲沒有變差。乾淨網路下每秒約有 3 次因為抖動而多送的 NACK，重傳量很小。

## 改版前（`main`）vs 改版後，同一個區網（2026-11-19 10:30–11:00）

- **改版前**：`main`（5121287）加上量測需要的 testsrc、overlay 和直通模式，其他不動。分支 `measure/main-baseline`（已推到 GitHub，不打算合併）。CLI 編碼器、30fps、RTP。
- **改版後**：`feat/stream-resilience`（1a24379）。libav、60fps、WebCodecs + FEC + FRAG_NACK，ABR 上限 3 Mbps，跟 `main` 固定的 3 Mbps 相同。
- **觀眾**：這台電腦上的 Chrome（分頁在前景）和 Pixel 手機，手機透過家用 Wi-Fi 連到同一個區網。
- **量測方式**：新舊版交替各量 3 輪（舊、新、舊、新、舊、新），每輪 50 次按鍵。用 `scripts/latency/measure-btn.js`，雜訊校正改取第 90 百分位；25ms 以下視為雜訊誤觸發，予以排除。

| 觀眾 | 版本 | 每輪 p50 | 合併 p50 | 合併 p90 |
|---|---|---|---|---|
| 電腦 Chrome | 改版前 | 132 / 132 / 132 | **132** | 155 |
| 電腦 Chrome | 改版後 | 47 / 60 / 42 | **48** | 67 |
| 手機 Wi-Fi | 改版前 | 169 / 171 / 170 | **169** | 194 |
| 手機 Wi-Fi | 改版後 | 89 / 97 / 90 | **92** | 127 |

單位 ms。改版後電腦端少了約 64%，手機端少了約 46%。以上都包含 overlay 測試路徑的固定成本（約 35ms）。

注意事項：
- 電腦 Chrome 的分頁要在前景。同一個設定下，分頁不在前景時量到 79～114ms。
- 手機在交替量測之前，有兩輪新版量到 211 和 146ms，原因沒查到。當時逐段拆開量：網路、host 送出佇列、手機解碼和顯示都正常，但加總只有約 100ms。之後的交替量測很穩定（89～97ms）。推測是 Wi-Fi 狀況，或頁面剛載入時的狀態，需要再觀察。
- 手機頁面剛載入的頭幾十秒，量測區的雜訊偶爾會很高，兩個版本都會。腳本現在會自動重試校正。

## 輸入端：Switch 2 gateway（待實機量測）

`input.target: gateway` 透過 USB serial 驅動 ESP32-C6 gateway，接法見 `docs/switch2-gateway.md`。目前只有單元測試，還沒有實機數字。

拆成兩段來看：

- **host 內部**：metrics 的 `input` 段，從 DataChannel 解出指令到寫進 serial。主要是限速的等待，`rate_hz: 150` 最多約 6.7ms。
- **gateway 裝置內**：metrics 的 `daemon` 段，取自 gateway status 回報的 `last_receive_to_submit_us`。事件驅動韌體在 5ms 節流內就會送出 report，不含 BLE 空中傳輸。韌體修正後，每個輸入只記第一次送出的時間。

端到端量法：
- 擷取卡拍 Switch 2 畫面，挑一個按下就有明顯變化的畫面，例如主選單游標移動。
- 用 `scripts/latency/` 的像素偵測，在游標區域量「網頁按下 → 畫面變化」。
- 結果減掉「只看影像」的延遲，就是輸入路徑的成本。
- 再換成 `input.target: nxbt` 用同樣方法量一次，兩者對比。

### 實測：Switch 2 主選單（2026-11-19 17:21，桌機 Chrome，同一台機器）

設定：
- 擷取：GC553，`capture.source: dshow`，720p60 NV12。
- 編碼：libav，60fps，ABR 上限 8Mbps。
- 傳輸：WebCodecs。
- 輸入：gateway（COM4），事件驅動韌體。

量法：`scripts/latency/measure-switch-menu.js` 在主選單交替按十字鍵右／左，偵測選取框上方遊戲名稱那一行的像素變化，量從網頁按下到畫面改變的時間。量 30 次。

| p50 | p90 | max | 逾時 |
|---|---|---|---|
| 154ms | 164ms | 167ms | 0 |

另有一筆 33ms 的離群值，判定為畫面其他動畫，不列入。

拆解：

| 段落 | 耗時 |
|---|---|
| 瀏覽器 → host | 約 0.4ms（RTT） |
| host 排隊 | p50 約 0，p95 5ms |
| gateway 收到 → 送出 | p50 1.8ms，p95 3.9ms |
| BLE | ≤ 5ms |
| 影像：編碼 | 0.5ms |
| 影像：送出 | 4ms |
| 影像：解碼 | 0.3ms |
| 擷取等待、顯示等待 | 各 ≤ 1 張（約 17ms） |
| 其餘（Switch 主選單反應、HDMI 輸出、GC553 擷取卡本身） | 約 110–130ms |

輸入路徑本身只占約 5–10ms。主選單的 UI 反應偏慢，遊戲內的數字要另外量。

量測陷阱：
- 選取框的發光邊框會持續變色，不能拿來偵測，要改看遊戲名稱那一行。
- 同時開著第二個觀看頁、又把它切到背景時，背景分頁停止讀取資料，ABR 會依「最差的觀眾」把所有人的位元率一起拉低（降到 1Mbps），量到的數字不可信。

### 實測：Switch 2 主選單影像＋聲音（2026-11-19 17:32–17:43，桌機 Chrome & Pixel 10 Pro Wi-Fi）

設定與 17:21 相同：GC553（`dshow` 720p60 NV12 + `dshow` 48kHz Opus）、libav 60fps、WebCodecs、ESP32-C6 gateway（`COM4`，`sdkconfig.usb_only` 事件驅動韌體）。

量法：`scripts/latency/measure-switch-av.js` 同時偵測：
- **影像**：遊戲名稱那一行像素變化（rAF）。
- **聲音**：透過 Web Audio `AnalyserNode`（2ms 輪詢）量選單移動音效 RMS 超過門檻的時間。此為**瀏覽器解碼端**延遲；到實際喇叭出聲還要加上 `AudioContext.outputLatency`（Windows WASAPI 約 40ms、Pixel Android 約 24ms）。
- 每組 30 次按鍵（`KeyH` / `KeyF` 交替）。部分試次因主選單背景動畫提早觸發（桌機 `<100ms`、手機 `<100ms`），下表同時列出全部樣本與排除提早觸發後的 clean p50。

#### 結果總表（單位 ms）

| 觀眾 | `host-libav` 狀態 | 影像 p50（clean） | 影像 p90 | 聲音 p50（解碼端） | 聲音 p90 | 音畫差（音 − 影） | 喇叭額外延遲 |
|---|---|---|---|---|---|---|---|
| 桌機 Chrome | **剛重啟（fresh）** | **156**（157） | 174 | **250** | 263 | **+93** | +40（base 10） |
| 桌機 Chrome | 運行 ~12 分鐘後 | **258**（262） | 276 | **247** | 259 | −15 | +40（base 10） |
| Pixel 10 Pro（Wi-Fi） | **剛重啟（fresh）** | **195**（201） | 241 | **350**\* | 370 | **+156** | +24（base 5） |
| Pixel 10 Pro（Wi-Fi） | 運行 ~16 分鐘後 | **295**（305） | 332 | **302** | 333 | +9 | +24（base 5） |

\* Pixel 10 Pro（fresh）的聲音樣本在前 5 次為 239–265ms（p50 **263ms**，僅比桌機多 ~13ms），第 7 次起因 Wi-Fi 抖動跳升至 ~325ms，第 12 次逾時後再升至 ~355ms 並維持到結束（見下方原因 3）。

#### 與模擬畫面（`testsrc` + `overlay`）比較

| 觀眾 | `testsrc` + `overlay`（60fps WebCodecs） | 實機 Switch 2 + GC553（fresh 影像） | 差值 |
|---|---|---|---|
| 桌機 Chrome | p50 **48 ms**（扣 overlay ~35ms ≈ 13–20ms） | p50 **156 ms** | **+108 ms**（相較純串流約 +135ms） |
| Pixel 10 Pro（Wi-Fi） | p50 **92 ms** | p50 **195 ms**（clean 201） | **+103 ms** |

- 桌機 → 手機 Wi-Fi 的增量在模擬與實機完全一致（`testsrc` +44ms，實機 fresh +39～44ms）。
- 實機 fresh 多出的 ~135ms 主要來自 Switch 2 主選單 UI 反應、HDMI 輸出與 GC553 擷取卡硬體管線，而非串流或 ESP32-C6 輸入路徑（輸入路徑本身僅 ~5–10ms）。

#### 排查出的三個延遲問題

1. **`dshow` 影像管線隨時間漂移累積約 +100ms（已由 A/B 重啟證實）**
   - `host-libav` 跑 12–16 分鐘後，桌機影像從 156 → 258ms（+102ms）、手機影像從 195 → 295ms（+100ms），重啟 `host-libav` 立刻回到 156ms / 195ms；同期桌機聲音完全沒變（247 vs 250ms），鎖定問題只在影像擷取管線。
   - 原因：`playgate-host/internal/capture/testsrc/source.go` 啟動 ffmpeg `dshow` 時設了 `-rtbufsize 16M`（720p NV12 單張 1.38MB，約可囤 11.5 張 ≈ 190ms），卻未加 `-fflags nobuffer -flags low_delay`。當擷取卡實際更新率（如 59.94Hz）與 Go 讀取節奏稍有落差或短暫停頓，buffer 會逐漸填滿並永久增加 ~100ms+ 延遲。
2. **聲音在剛啟動時就比影像慢 ~93ms（到 Windows 喇叭約慢 ~133ms）**
   - 剛重啟時桌機影像 156ms，但聲音解碼端已是 250ms（+93ms）。
   - 原因：`playgate-host/internal/audio/opus/source.go` 啟動 ffmpeg `dshow` 擷音時缺少 `-fflags nobuffer -probesize 32 -analyzeduration 0`，且 `-audio_buffer_size 20`、`-frame_duration 20`、`-page_duration 20000` 各帶 20ms 緩衝，加上 Chrome WebRTC 音訊 jitter buffer（~20–40ms）。
3. **手機 Wi-Fi 音訊 jitter buffer（NetEQ）遇抖動後拉高且回落慢**
   - Pixel fresh 實測中，聲音延遲從 263ms 階梯式升至 325ms → 355ms，而 WebCodecs 影像沒有 NetEQ 緩衝仍維持 ~200ms，導致手機端音畫不同步擴大至 +156ms（喇叭端約 +180ms）。

#### 修正後 A/B 實測（2026-11-19 17:50，桌機 Chrome）

修正內容：
- 影像 `dshow`（`internal/capture/testsrc/source.go`）：加上 `-fflags nobuffer -flags low_delay -probesize 32 -analyzeduration 0`，`-rtbufsize` 依解析度縮為約 3 張（720p 為 `4M`）。
- 音訊 `dshow`/Opus/Ogg（`internal/audio/opus/source.go`）：加上同樣的低延遲參數，並將 `audio_buffer_size`、`frame_duration`、`page_duration` 從 20ms 降至 **10ms**。

| 觀眾 | 項目（剛重啟，n=30） | 修正前 | 修正後（`8c4e115`） | 改善 |
|---|---|---|---|---|
| 桌機 Chrome | 影像 p50（clean p50） | 156ms（157ms） | **126ms（133ms）** | **−24～30ms** |
| 桌機 Chrome | 影像 p90 / max | 174 / 181ms | **137 / 148ms** | −37 / −33ms |
| 桌機 Chrome | 聲音 p50（解碼端） | 250ms | **194ms** | **−56ms** |
| 桌機 Chrome | 聲音 p90 / max | 263 / 274ms | **203 / 211ms** | −60 / −63ms |
| 桌機 Chrome | 音畫差（音 − 影，clean p50） | +93ms | **+60ms** | **−33ms** |
| Pixel 10 Pro（Wi-Fi） | 影像 p50（clean p50） | 195ms（201ms） | **171ms（176ms）** | **−24～25ms** |
| Pixel 10 Pro（Wi-Fi） | 影像 p90 / max | 241 / 295ms | **251 / 311ms** | +10 / +16ms（Wi-Fi 抖動） |
| Pixel 10 Pro（Wi-Fi） | 聲音 p50（解碼端） | 350ms（前 5 次 263ms） | **279ms（前 5 次 247ms）** | **−71ms** |
| Pixel 10 Pro（Wi-Fi） | 聲音 p90 / max | 370 / 375ms | **338 / 348ms** | −32 / −27ms |
| Pixel 10 Pro（Wi-Fi） | 音畫差（音 − 影，p50） | +156ms | **+109ms（clean ~+92ms）** | **−47ms** |

99 分鐘連續長跑（17:59–19:39，桌機 Chrome，n=30）：聲音維持 **p50 194ms**（零漂移）；影像在 `rtbufsize 4M` 下封頂於 **p50 193ms（clean 195ms）**（相較舊版 `16M` 12 分鐘就漂到 258ms 大幅收斂；後續再補上 `-fps_mode passthrough` 避免 ffmpeg CFR 配速囤積）。




## GC553 擷取卡本身的延遲（2026-11-21，`scripts/latency/caplat.c`）

> `scripts/latency/caplat.c` 已被 [caplat](https://github.com/virgil724/caplat) 取代（Go 寫的，支援 MF、DirectShow、ffmpeg 三種讀法，以及聲音和影音同步量測），新數字見下一節。

方法：
- 電腦的 HDMI 輸出接到 GC553 的輸入（延伸桌面的 3 號螢幕，輸出 3840×2160@60）。
- caplat 用 D3D11 flip-model swap chain 在那個螢幕上逐張畫遞增編號的條碼，用 `GetFrameStatistics` 取得每張實際開始送出的 vblank 時間（QPC）。
- 同時用 ffmpeg dshow 讀回擷取卡畫面，解出條碼並記錄收到的時間（QPC）。
- 延遲 = 收到 − vblank，兩邊用同一個時鐘，電腦端的合成和等 vsync 不會算進去。每輪 300–600 張。

| ffmpeg 參數 | p50 | p10–p90 | 備註 |
|---|---|---|---|
| 舊版（只有 `-rtbufsize 16M`） | 59.4 或 76ms | 約 17ms | 每輪由啟動時決定是否多一張；8 輪裡 2 輪是 76ms |
| `-fflags nobuffer -probesize 32 -analyzeduration 0` | 35.0ms | 0.3ms | 沒加 `-fps_mode passthrough` 時，偶爾仍會在中途積壓兩張不退 |
| host 現行參數（上列 + `-flags low_delay` + `-fps_mode passthrough`） | **35.0ms** | **0.3ms** | 10 輪全部 35.0ms，最慢 37.8ms |

- 1080p 擷取（舊參數）61ms，和 720p 同級，GC553 的縮放幾乎不花時間。
- GC553 本身約 35ms：收完一整張畫面約 16.7ms，加上約 18ms 的處理、USB 傳輸和驅動交付。
- 舊參數多出的 25–40ms 和約 17ms 的抖動，都是 ffmpeg 開啟裝置時預讀、輸入端緩衝造成的；這段積壓之後讀寫同速，永遠清不掉。
- 影像擷取在 `3d9e903` 已經改用新參數；聲音的 ffmpeg 輸入（ALSA 和 dshow）原本就統一加了同一組參數。
- 前面 2026-11-19 主選單那次 154ms 是用舊參數量的，其中 GC553 約占 59–76ms。改用新參數後，扣掉 GC553 的 35ms、輸入 5–10ms 和串流 15–25ms，Switch 主選單反應加 HDMI 約占 60–75ms。

## 程式內擷取：MF 影像 + WASAPI 聲音（2026-11-21，`feat/inprocess-capture`）

Windows host 新增 `capture.source: mf` 和 `audio.format: wasapi`（需 `-tags libav`），不再開 ffmpeg 子程序讀擷取卡：影像用 Media Foundation 的 `IMFSourceReader` 直接讀 NV12，聲音用 WASAPI 讀 float32、在程式內用 libav 的 libopus 編碼。兩者都用 caplat 公開的 `capture/mf`、`capture/wasapi` 套件。

### 擷取端拆解（caplat，電腦 HDMI → GC553，vblank → 程式收到）

| 讀法 | 影像 p50 | p10–p90 |
|---|---|---|
| Media Foundation，程式內 | **18.1ms** | 0.2ms |
| DirectShow，程式內 | 17.9ms | 0.1ms |
| ffmpeg + DirectShow（host 舊參數） | 35.2ms | 0.3ms |

- 省下來的那一張畫面來自拿掉 ffmpeg，不是 MF 比 DirectShow 快：兩種 API 程式內直接讀一樣快，是 ffmpeg 多囤了一張。
- 選 MF 是因為它有驅動的擷取時間戳（`MFSampleExtension_DeviceTimestamp`，和 QPC 同一個時鐘）、程式碼較簡單；要支援虛擬攝影機（只有 DirectShow 介面）時再加程式內 DirectShow。
- 聲音：WASAPI 程式內讀，驅動時間戳 → 程式收到 p50 約 1.5ms（獨佔，3ms 週期）或 9.4ms（共享，10ms 週期）；ffmpeg dshow `-audio_buffer_size 10` 是 21.6ms。

### 端到端 A/B（Switch 主選單，桌機 Chrome，每輪 30 次，`scripts/latency/measure-switch-av.js`）

舊 = `dshow`/`dshow`（ffmpeg），新 = `mf`/`wasapi`。GC553 中途卡死過一次、軟體重開後兩邊都慢了約一張畫面，所以只在同一個卡片狀態內比較。

| 卡片狀態 | 設定 | 影像 p50 | 聲音 p50 | 音畫差 |
|---|---|---|---|---|
| 重開前 | 舊（3 輪） | 127 / 129 / 129 | 195 / 191 / 203 | 約 +70 |
| 重開前 | 新，WASAPI 獨佔（2 輪） | **112 / 112** | **163 / 167** | 約 +52 |
| 重開後 | 舊 | 145 | 195 | +50 |
| 重開後 | 新，WASAPI 共享（2 輪） | **126 / 127** | **165 / 167** | 約 +40 |

- 影像快約 17–19ms（剛好一張畫面），聲音快約 30ms，音畫差縮小 10–20ms。
- 共享模式和獨佔模式的端到端聲音延遲一樣，獨佔的 3ms 週期在 Chrome 的 jitter buffer 之後看不出差別。
- **GC553 卡死**：在 MF + WASAPI 獨佔下跑第 3 輪時，影像和聲音同時停止，之後 ffmpeg 也讀不到（I/O error），PnP 狀態仍顯示正常。卡死前聲音延遲已升到 360–490ms。不用拔插的復原方式（系統管理員）：`pnputil /restart-device "USB\VID_07CA&PID_0553\<序號>"`。因此 `exclusive` 在 `config.example.yaml` 標為實驗性，預設共享。

### 長時間跑（MF + WASAPI 共享，31 分鐘，每 5 分鐘量一輪）

| 分鐘 | 影像 p50 / p90 | 聲音 p50 / p90 | 音畫差 p50 |
|---|---|---|---|
| 6 | 129 / 138 | 158 / 166 | +29 |
| 11 | 125 / 136 | 158 / 171 | +33 |
| 16 | 128 / 140 | 163 / 175 | +35 |
| 21 | 127 / 136 | 163 / 175 | +39 |
| 26 | 131 / 138 | 167 / 178 | +38 |
| 31 | 130 / 138 | 159 / 170 | +31 |

- 影像、聲音都沒有漂移；舊版 ffmpeg dshow 跑 12–16 分鐘後影像多約 100ms 的問題沒有再出現。
- 整段 0 個 WARN、0 次重開，GC553 沒有再卡死。host 編碼 p50 0.55ms、p95 1.5ms。
- 第 6 分鐘那輪聲音有 11 次逾時：選單音效太小聲，低於偵測門檻（RMS 0.015），兩種設定都會發生，與 host 無關。
