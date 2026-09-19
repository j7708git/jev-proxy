# jev-proxy

OpenAI 相容的 passthrough proxy：不做路由、不擋回覆、不修改任何內容，只在每輪最終文字回覆產出後，以對話上下文為依據非同步呼叫 Jev 打一個 0–3 的品質分（外加獨立的安全閘），寫入 JSONL。

架構規劃與 rubric 定義見 `../docs/jev-score-proxy-plan.html`（rubric v1，四級 + 安全閘 + confidence 把手）。

## 狀態

- **M1 透明 passthrough**：完成 — ReverseProxy + 串流 buffer + request_id + JSONL 事件 + /healthz + /metrics
- **M2 評分上線**：完成 — Jev adapter（TypeSafe + OpenRouter 兩 provider）、rubric v1、queue/worker、加權分計算、/v1/scores 查詢、skipped/error 事件
  - 2026-09-19 修正兩處：OpenRouter 預設 model id 補上 `~` 命名空間（先前 `typesafe/jev-latest` 會 400）；串流模式補上 `sample_rate` 抽樣檢查（先前即使 rate=0 仍一律入隊評分）
  - ✅ 真實 e2e 已通過（2026-09-19，OpenRouter）：串流直通 → 非同步評分 `status: ok`，`jev_model = typesafe/jev-1.13-20260917`，weighted 自算與 API 彙總交叉吻合（2.28 = 2.28）；該輪 confidence 0.68 < 0.7 → `low_confidence: true` 壓住旗標，正是漂移把手的 live 範例（見 `scores-e2e-2026-09.jsonl`）
  - 2026-09-19 加專案 `.env`：啟動自動載入 `./.env`（或 `-env` 指定／config 同目錄），環境變數優先於檔案；金鑰不再依賴 shell
  - 免 key 迴歸：`python e2e/fake_upstream.py` + `python e2e/fake_jev.py` + `./jev-proxy.exe -config e2e/config-e2e-stub.yaml`（stub 會回顯 model，證明送出的 slug 正確）
- **M3 校準 harness**：完成，rubric 定版 **v1.0** — `./jev-proxy.exe calibrate --set tests --rubric v1.0 [--json out.json]`，30 筆案例走的是生產同一條管線（`BuildState` → Jev → 門檻）。2026-09-19 共跑 4 輪，品質閘門穩定全過：error recall 11/12（91.7%）、乾淨樣本 L0 誤報 0/18、Spearman 0.65–0.69、safety 種子 2/2；單輪總成本 < $0.002、約 10 秒。認證報告：`tests/calibration-v1.0.json`
- 演進路線（model cascade / escalation）依規劃文件屬於後續階段，尚未實作

## 快速開始

```sh
# 建置（工具鏈在 ../go/bin/go.exe；或用系統 PATH 的 go）
go build -o jev-proxy.exe ./cmd/jev-proxy

# 放金鑰：把 .env.example 複製成 .env、填真值（.env 已在 .gitignore）
cp .env.example .env      # 然後編輯 OPENROUTER_API_KEY=...
# 或走環境變數（環境優先於 .env，可臨時覆寫）：
#   OPENROUTER_API_KEY=...   provider: openrouter
#   TYPESAFE_API_KEY=...     provider: typesafe

# 跑起來（啟動會自動載入 ./.env；-env 可換路徑）
./jev-proxy.exe -config config.yaml
```

把客戶端（Hermes / ZCode / 任何 OpenAI SDK）的 `base_url` 指向 `http://localhost:8080/v1` 即可。主流程零修改、串流零延遲。

## 行為

| 情境 | 行為 |
|------|------|
| 串流回覆（預設） | 位元組原樣轉發；`data: [DONE]` 後組出全文，非同步評分，分數只進 JSONL 與查詢 API |
| 非串流回覆 | 同步評分後再回應（加 0.1–0.5s），附 `X-Jev-Weighted` / `X-Jev-Safety` / `X-Jev-Confidence`，低分再加 `X-Jev-Flag: review` |
| 純 tool-call 輪（content 空） | 不評分，計入 `no_content_total` |
| 串流沒等到 `[DONE]` / 中斷 | 記 `skipped` 事件（`stream_no_done` / `stream_interrupted`） |
| 評分佇列滿 | 丟棄評分，記 `skipped`（`queue_full`）；永不擋主流程 |
| Jev 呼叫失敗 | 記 `error` 事件（weighted 為 null）；429/529 指數退避重試 |

兩種模式都會附 `X-Jev-Request-Id`（proxy 生成的 UUID，查分的 key）。

## 端點

- `POST /v1/chat/completions` — 純 passthrough + 非同步評分
- `GET /v1/scores/{response_id}` — 取該回覆的評分事件
- `GET /metrics` — 計數器、Jev 延遲分位（p50/p95）、加權分分佈（`score_level_l0..l3_total`）
- `GET /healthz`

## 評分事件（scores-YYYY-MM.jsonl）

```json
{
  "ts": "2026-09-18T21:50:00+08:00",
  "response_id": "<X-Jev-Request-Id>",
  "upstream_id": "chatcmpl-…",
  "upstream_model": "gpt-…",
  "rubric_version": "v1",
  "jev_model": "typesafe/jev-1.13-…",
  "weighted": 2.37,            // proxy 由四級機率算出：Σ p(Li)×i
  "api_score": 2.4,            // Jev 自帶的彙總分，留作交叉檢查
  "level_probs": [0.02, 0.06, 0.31, 0.61],
  "confidence": 0.82,          // judge 的校準自評
  "low_confidence": false,     // confidence < min_confidence：照記錄但不觸發旗標
  "safety_prob": 0.005,
  "safety_flag": false,
  "flag_review": false,
  "suspect_l0": false,
  "status": "ok",              // ok | skipped | error
  "reason": "",                // skipped 原因
  "latency_ms": 213,
  "reply_sha256": "…",
  "reply": "完整回覆（供複核）",
  "error": null
}
```

Jev API 實測 schema（2026-09-18，`typesafe/jev-1.13-20260917`）：score 型答案的 `probabilities` 以**等級索引字串**為 key（`"0"`–`"3"`），另有 API 自己算的 `score` 與校準過的 `confidence`；noul 型答案的機率欄位名為 `noul`。proxy 自己重算加權分，門檻語義因此綁定 rubric 版本而非 API 彙總。

## 設定

見 `config.yaml` 內註解（非密鑰設定）。金鑰類放本地 `.env`（範本 `.env.example`、載入器 `internal/dotenv`）：啟動依序找 `-env` 指定路徑 → `./.env` → config 同目錄；**shell 已設的環境變數蓋過檔案值**。`.env` 與 `scores-*.jsonl`（內含完整回覆）都已被 `.gitignore` 排除。熱參數（改檔重啟即生效）：`sample_rate`、`context_turns`、三個門檻、`min_confidence`、queue 容量。

## 校準（M3）

```sh
./jev-proxy.exe calibrate --set tests --rubric v1.0 --json tests/calibration-v1.0.json
```

`tests/cases.jsonl` 依規劃文件為 30 筆：10 乾淨（人工 L2/L3）、10 種子變異（應判 L0/L1，含 2 筆 safety 洩漏種子）、10 歷史真實（繁中＋代碼混合）。校準與生產共用同一條管線與同一個 rubric 常數——不存在「測試用的那版規則」。quality 閘門（召回／誤報／Spearman／safety）決定定版與否；cost、p95 延遲是 ops 訊號，只警告不動 rubric（改措辭救不了網路抖動）。

**三個人必須知道的實測發現（v1.0 不修，如實記錄）：**

1. **驗算錯誤抓不到**：`259×4=1026`（實為 1036）被判 L3。judge 沒有計算器，這是規劃書預留的天花板，屬於「一致性評審」而非事實查核的邊界。
2. **L2/L3 邊界系統性偏寬**：多筆「普通但正確」的回覆被抬成 L3（加權分 2.4–2.7）。方向安全——不會把壞的放進好的。
3. **信心分布很有結構**：抓到錯誤時 confidence 0.84–0.98，評好回覆時普遍 0.2–0.7（約 6 成事件低於 min_confidence=0.7）。這正是「單向閘」哲學的實證：低分可信、高分只是參考。**做 cascade 前必須重審 min_confidence 預設值**——0.7 會壓掉多數分數。

## 限制

- judge 只依 state（對話上下文 + 回覆）判定，抓不到與上下文無關的外部事實錯誤——這是一致性評審，不是事實查核
- 串流模式的分數無法回填 header（HTTP header 先於 body 送出），只能走 JSONL 與查詢 API
- 非同步評分在 proxy 重啟時，佇列內未評事件會丟失（分數是遙測，不是交易）
