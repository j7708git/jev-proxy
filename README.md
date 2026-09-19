# jev-proxy

OpenAI 相容代理，兩種模式共用同一條評分支線——每輪最終文字回覆產出後，以對話上下文為依據非同步呼叫 Jev 打一個 0–3 的品質分（外加獨立的安全閘），寫入 JSONL。**任何模式下都不修改、不擋、不改寫回覆內容。**

- **gateway（預設範例）**：多供應商金庫。各家 provider 的 key 集中放在本機 `.env`，agent 只需要「選模型」；proxy 按模型名路由到 openrouter / deepseek 官方 / nous…並注入對應金鑰
- **passthrough（legacy）**：單一 `upstream`，客戶端自帶的 Authorization 原樣直通，proxy 只做評分遙測

架構規劃與 rubric 定義見 `../docs/jev-score-proxy-plan.html`（四級 + 安全閘 + confidence 把手；其中「不做路由」的 v1 非目標已被 gateway 模式取代，金鑰管理見本檔）。

## 狀態

- **M1 透明 passthrough**：完成 — ReverseProxy + 串流 buffer + request_id + JSONL 事件 + /healthz + /metrics
- **M2 評分上線**：完成 — Jev adapter（TypeSafe + OpenRouter）、rubric、queue/worker、加權分、/v1/scores、skipped/error 事件；真實 OpenRouter e2e 通過（`status: ok`，weighted 與 API 彙總交叉吻合）
- **M3 校準 harness**：完成，rubric 定版 **v1.0** — 30 筆案例走生產同一條管線；4 輪品質閘門穩定全過（recall 11/12、誤報 0/18、Spearman 0.65–0.69、safety 2/2）。認證報告：`tests/calibration-v1.0.json`
- **gateway 多供應商路由**（2026-09-19 新增）：providers/routes/default 解析、key 注入、provider 前綴命名空間、`GET /v1/models` 合併清單、可選客戶端門鎖；legacy passthrough 保留為相容模式
- **M4 觀測與行動** / **cascade**：未開始（見規劃文件演進路線）

## 快速開始

```sh
go build -o jev-proxy.exe ./cmd/jev-proxy        # 工具鏈在 ../go/bin/go.exe

cp .env.example .env     # 填入各家 key：OPENROUTER/DEEPSEEK/NOUS/JEV_CLIENT_KEY...
./jev-proxy.exe -config config.yaml              # 自動載入 ./.env（-env 可換路徑）
```

Agent 端：`base_url` 指向 `http://localhost:8080/v1`，`Authorization` 填 `JEV_CLIENT_KEY` 的值（若啟用了門鎖；沒啟用就隨意填），`model` 從 `GET /v1/models` 的清單裡挑——名字自帶 provider 命名空間，選誰就是誰。

## 多供應商路由（gateway）

### 解析優先序（先命中先贏）

1. **`routes` 名稱表** — 對**完整模型名**做 glob（`*` 跨 `/`）的有序規則，可用 `model:` 做轉發改名。這是逃生門與捷徑（例：`{ match: "deepseek-chat", provider: deepseek }`）
2. **provider 前綴** — 第一段等於已註冊的 provider 名 → 轉發給它並**剝掉前綴**：`nous/deepseek/deepseek-r1` → Nous 收到 `deepseek/deepseek-r1`；`openrouter/deepseek/deepseek-chat-v3` → OpenRouter 收到 `deepseek/deepseek-chat-v3`（只剝第一段，內部斜線原樣保留）
3. **`default`** — 全不命中時兜底（範例為 openrouter，幾乎什麼都收）

### Nous / OpenRouter 同 ID 碰撞怎麼解

兩家大量模型 ID 相同（`deepseek/deepseek-chat-v3`、`meta-llama/llama-3.1-405b`…）。**解法是命名空間成為一級公民**：`GET /v1/models` 回的是**合併＋前綴化**的清單（`alpha/gpt-4o`、`nous/gpt-4o`…），agent 從清單選模型時，選的名字本身已經唯一決定了 provider。手打模型名才需要注意：`deepseek/...` 若剛好被同名 provider 搶走，想給 OpenRouter 就明寫 `openrouter/deepseek/...`（或加一條 route 規則蓋掉）。

### 金鑰流

- provider key 只存在 `.env`（→ 環境變數），**只在转發瞬间注入对应请求**，client 傳來的 Authorization 一律被蓋掉
- 客戶端從頭到尾看不到任何 provider key；proxy 掛了就是網路掛了，不留憑證
- `JEV_CLIENT_KEY` 有值＝啟用門鎖：`/v1/*` 都要帶它（常數時間比對），防止本機其他程式或區網白嫖金庫；預設 `listen: 127.0.0.1:8080` 只綁本機
- 某家 key 沒填：開機警告、其他家照常、打到那家才報錯（缺哪些會印在啟動日誌）

## 客戶端接法（dsh / Hermes）

共通前提：**先啟動 jev-proxy**；agent 端 `Authorization` 填 `JEV_CLIENT_KEY` 的值（門鎖沒開則隨意填非空字串——pi-ai 的 OpenAI 協定不允許零憑證，佔位即可）；模型名**用 `GET /v1/models` 清單上的 namespace 名**（`openrouter/…`、`nous/…`，或 `deepseek-chat` 這種被 routes 直收的）。

### dsh（本 harness）

`~\.dsh\settings.yaml` 的 `llm-pi-ai.providers` 加一條自訂 route（pi-ai 內建目錄外的 route 必填 `api`、`baseURL`、非空 `models`）：

```yaml
llm-pi-ai:
  providers:
    jev-gateway:
      displayName: "Jev Gateway (scored)"
      api: openai-completions          # 必填：OpenAI chat 協定
      baseURL: http://127.0.0.1:8080/v1
      apiKeyEnv: JEV_CLIENT_KEY        # 憑證引用：先 credentials 儲存區，再環境變數
      models:
        - id: openrouter/deepseek/deepseek-v4-flash
          name: "DeepSeek V4 Flash (經 OpenRouter，含評分)"
        - id: nous/deepseek/deepseek-r1-0528
          name: "DeepSeek R1 0528 (Nous 直連，含評分)"
        - id: deepseek-chat
          name: "DeepSeek Chat (官方直連，含評分)"
```

金鑰二擇一：① Web **Settings → Models** 頁在該 provider 的 API key 欄貼值——存進 credentials、`settings.yaml` 不落明文，引用名即 `JEV_CLIENT_KEY`；② 系統環境變數 `setx JEV_CLIENT_KEY "<值>"` 後**重開 dsh**（setx 對已啟動進程無效）。`models` 照 `/v1/models` 的輸出增刪。踩過的人會遇到的錯碼：`MISSING_CREDENTIAL`＝引用解析為空（key 沒存/環境沒生效）、`UNKNOWN_MODEL`＝models 清單沒列那顆。

### Hermes

`%LOCALAPPDATA%\hermes\config.yaml` 的 `providers:` 加一條（與你現有的 `openrouter_custom` 同格式）：

```yaml
providers:
  jev:
    name: jev
    base_url: http://127.0.0.1:8080/v1
    key_env: JEV_CLIENT_KEY
    discover_models: true      # 預設即 true；直接抓 gateway 的 namespace 合併清單
    models:
      openrouter/deepseek/deepseek-v4-flash: {}   # 最低兜底，discover 會補齊其餘
```

同一個值放進 Hermes 自己的 `.env`（與 `config.yaml` 同目錄，`$HERMES_HOME\.env`；`key_env` 解析優先讀它）：

```
JEV_CLIENT_KEY=<與 jev-proxy\.env 裡同一個值>
```

要預設走閘道就把頂部 `model:` 的 `provider: jev`、`default:` 填清單上的 namespace 名，或直接在 UI 的模型選單換。

### 接完驗收（兩家通用）

```sh
# 1) 清單與門鎖
curl -H "Authorization: Bearer <JEV_CLIENT_KEY>" http://127.0.0.1:8080/v1/models
# 2) 一輪對話看路由與評分
curl -N -H "Authorization: Bearer <JEV_CLIENT_KEY>" -H "Content-Type: application/json" \
  -d '{"model":"openrouter/deepseek/deepseek-v4-flash","stream":true,"messages":[{"role":"user","content":"hi"}]}' \
  http://127.0.0.1:8080/v1/chat/completions -D -
#    回應標頭應有 X-Jev-Provider: openrouter；數秒後 scores-YYYY-MM.jsonl 多一筆
#    {"provider":"openrouter","status":"ok",weighted…}
# 3) 總量
curl http://127.0.0.1:8080/metrics   # provider_requests_<name>_total 隨對話递增
```

Agent 側若連不上：先確認 proxy 進程活著（connection refused＝沒啟動/埠错）、401＝兩邊 `JEV_CLIENT_KEY` 不同值、模型 404 但清單有＝名字漏了 namespace 前綴。

## 行為

| 情境 | 行為 |
|------|------|
| 串流回覆（預設） | 位元組原樣轉發；`data: [DONE]` 後組出全文，非同步評分，分數只進 JSONL 與查詢 API |
| 非串流回覆 | 同步評分後再回應（加 0.1–0.5s），附 `X-Jev-Weighted` / `X-Jev-Safety` / `X-Jev-Confidence`，低分再加 `X-Jev-Flag: review` |
| gateway：模型解析 | 命中規則／前綴／default；前綴命中時轉發體改寫（Content-Length 同步）；回應附 `X-Jev-Provider` |
| gateway：模型名無解（沒設 default 時） | 400 `jev_proxy_route_error`，計入 `route_error_total` |
| 門鎖啟用且 key 不符 | 401（`/v1/*` 一律；`/healthz`、`/metrics` 不驗） |
| 純 tool-call 輪（content 空） | 不評分，計入 `no_content_total` |
| 串流沒等到 `[DONE]` / 中斷 | 記 `skipped` 事件（`stream_no_done` / `stream_interrupted`） |
| 評分佇列滿 | 丟棄評分，記 `skipped`（`queue_full`）；永不擋主流程 |
| Jev 呼叫失敗 | 記 `error` 事件（weighted 為 null）；429/529 指數退避重試 |

每種模式都會附 `X-Jev-Request-Id`（proxy 生成的 UUID，查分的 key）。

## 端點

- `POST /v1/chat/completions` — 路由（gateway）或直通（legacy）＋評分
- `GET /v1/models` — gateway：合併各家清單，`id` 帶 `provider/` 命名空間、另附 `jev_provider`；某家抓取失敗跳過並在 `X-Jev-Models-Partial` 列出；快取 5 分鐘
- `GET /v1/scores/{response_id}` — 取該回覆的評分事件
- `GET /metrics` — 計數器（含 `route_error_total`、`provider_requests_<name>_total`）、Jev 延遲 p50/p95、加權分分佈 `score_level_l0..l3_total`
- `GET /healthz`

## 評分事件（scores-YYYY-MM.jsonl）

```json
{
  "ts": "2026-09-19T21:50:00+08:00",
  "response_id": "<X-Jev-Request-Id>",
  "provider": "nous",              // gateway 路由結果（legacy 無此欄）
  "upstream_id": "chatcmpl-…",
  "upstream_model": "gpt-…",
  "rubric_version": "v1.0",
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

見 `config.yaml` 內註解（非密鑰設定）。金鑰類放本地 `.env`（範本 `.env.example`、載入器 `internal/dotenv`）：啟動依序找 `-env` 指定路徑 → `./.env` → config 同目錄；**shell 已設的環境變數蓋過檔案值**。`.env` 與 `scores-*.jsonl`（內含完整回覆）都已被 `.gitignore` 排除。熱參數（改檔重啟即生效）：路由表、provider 端點、`sample_rate`、`context_turns`、三個門檻、`min_confidence`、queue 容量。

### E2E（全免 key）

```sh
# legacy stub：python e2e/fake_upstream.py 9911; python e2e/fake_jev.py 9912
#   ./jev-proxy.exe -config e2e/config-e2e-stub.yaml          # :8082
# gateway：python e2e/fake_upstream.py 9911; python e2e/fake_upstream.py 9912; python e2e/fake_jev.py 9913
#   E2E_ALPHA_KEY=.. E2E_BETA_KEY=.. E2E_CLIENT_KEY=sk-test OPENROUTER_API_KEY=.. \
#   ./jev-proxy.exe -config e2e/config-e2e-gateway.yaml       # :8083
```

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
- gateway 金庫：`/v1/models` 依賴各家清單端點可用（失敗只降級該家）；未設 `JEV_CLIENT_KEY` 時同機任何程式可用你的 key——預設只綁 127.0.0.1 是本機信任邊界，跨機使用請務必開門鎖並自行處理傳輸安全
