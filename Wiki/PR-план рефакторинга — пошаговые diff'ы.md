---
title: PR-план рефакторинга — пошаговые diff'ы
type: proposal
tags: [ai-reviewer, ddd, refactoring, pr-plan, diff]
updated: 2026-06-20
status: proposal
---

# PR-план рефакторинга — пошаговые diff'ы

> **Суть:** конкретная, инкрементальная реализация [[Рефакторинг к DDD-пакетам]].
> Шесть PR от низкого риска к высокому; **каждый компилируется и проходит
> [[Fixture dry-run — тест фильтрации без токенов|fixture dry-run]]**. Diff'ы
> иллюстративны (точные символы из кода, но без претензии на компиляцию as-is).

## Карта PR
| PR | Цель | Риск | Главный выигрыш |
|---|---|---|---|
| PR-1 | вынести чистый `domain/codebase` | низкий | домен без зависимостей |
| PR-2 | `domain/review` + port'ы | низкий | инверсия зависимостей |
| PR-3 | единый источник формулы стоимости | низкий | фикс дубля ×3 |
| PR-4 | распил `NewRunConfig` | средний | убрать god-method |
| PR-5 | `modelclient.Pool` + контракт токенов + тесты провайдеров | средний | фикс семантики |
| PR-6 | enforcement JSON у Anthropic | средний | надёжность normalize |

---

## PR-1 — выделить `internal/domain/codebase` (чистое ядро)
**Файлы:** новый пакет; перенос `AnnotateDiff`, `ParseAnnotatedFileDiff`, `FilterSet`,
`FileContext`, `PRContext` из `context.go`. Без `os/exec` (git остаётся в infra).
```diff
+ internal/domain/codebase/diff.go       // AnnotateDiff, hunkHeaderRegexp, ParseAnnotatedFileDiff
+ internal/domain/codebase/filterset.go  // FilterSet, Matches, MatchesPath, Compile, pathIncluded
+ internal/domain/codebase/prcontext.go  // PRInfo, PRContext, FileContext, ChangedLineNumbers
- // context.go: оставить только git-IO (GetDiff, GetPRContext, GetFilesForPatterns)
```
**Проверка:** `go build ./... && go test ./...`; [[Fixture dry-run — тест фильтрации без токенов|fixture]]
не меняет вывод (поведение идентично).
**Почему первым:** эти функции уже почти чистые — минимальный риск, см.
[[Git-дифф и pathspec — добыча изменений]], [[FilterSet — управление стоимостью]].

---

## PR-2 — `domain/review` + port'ы (инверсия зависимостей)
Объявить интерфейсы в домене; infra их реализует.
```diff
+ // internal/domain/review/ports.go
+ package review
+ type ModelClient interface {
+     Generate(ctx context.Context, prompt string, maxTokens int) (ModelResult, error)
+     GenerateJSON(ctx context.Context, prompt string, maxTokens int) (ModelResult, error)
+ }
+ type PolicyStore interface { Personas() []Persona; Primers() []Primer; Waivers() []Waiver }
```
```diff
  // models.go (станет internal/infra/modelclient)
- type ModelClient interface { ... }   // удалить — теперь в domain/review
+ var _ review.ModelClient = (*OpenAIClient)(nil)   // compile-time проверка реализации
```
**Проверка:** компиляция; `go vet`. Поведение не меняется.
Связь: [[Model Access — ACL над провайдерами]] становится честным port/adapter.

---

## PR-3 — единый источник формулы стоимости (фикс дубля ×3)
Сейчас формула в `settings.go:138`, `main.go:341` и (input-only) в context-eval.
```diff
+ // internal/domain/review/cost.go
+ func (e RunLogEntry) Cost() float64 {
+     return float64(e.TokensIn)*e.InputPrice/1e6 +
+            float64(e.TokensOut+e.TokensReasoning)*e.OutputPrice/1e6
+ }
```
```diff
  // settings.go GetStatsString
- cost := (float64(entry.TokensIn) * entry.InputPrice / 1e6) + ...
+ cost := entry.Cost()
  // main.go generateReport
- cost := (float64(s.TokensIn) * s.InputPrice / 1e6) + ...
+ cost := s.Cost()
```
**Проверка:** `go test`; числовой вывод `stats.txt`/`report.md` не изменился.
Контекст: [[Отчёт и операбельность — report и finish-reason]],
[[Учёт и отчётность — RunResults и stats]].

---

## PR-4 — распил `NewRunConfig` (god-method → 3 роли)
```diff
+ // internal/app/plan.go
+ type Planner struct { cfg *Config; prInfo *PRInfo; global *PRContext }
+ func (p *Planner) Partition(personas []Persona) (pre, rev, post PersonaGroups)  // фаза 6
+ // internal/app/run.go
+ type RunService struct { store PolicyStore; models *modelclient.Pool; out *OutputHandler }
+ func (r *RunService) Execute(ctx, plan) (*RunResults, error)                    // стадии конвейера
```
```diff
  // settings.go
- func NewRunConfig(...) {  // 7 фаз: IO + планирование + клиенты — всё вместе }
+ func NewRunConfig(...) {  // только wiring: infra-конструкторы + new(Planner/RunService) }
```
**Проверка:** [[Fixture dry-run — тест фильтрации без токенов]] (проверяет именно деление
персон на группы — идеальный страховочный тест для этого PR). Связь:
[[Composition Root — NewRunConfig]].

---

## PR-5 — `modelclient.Pool` + контракт токенов + контрактные тесты
Централизовать клиентов и зафиксировать семантику токенов (см.
[[Провайдеры — таблица различий]]).
```diff
+ // internal/infra/modelclient/pool.go
+ type Pool struct { mu sync.Mutex; cache map[string]review.ModelClient }
+ func (p *Pool) Get(provider, model, reasoning string) (review.ModelClient, error)  // кэш по ключу
```
Контракт `ModelResult`: **`TokensOut` — output БЕЗ reasoning; `TokensReasoning` — отдельно.**
Привести адаптеры:
```diff
  // openai: completion_tokens включает reasoning → вычесть, чтобы не задваивать
- TokensOut: resp.Usage.CompletionTokens,
+ TokensOut: resp.Usage.CompletionTokens - reasoningTokens,
+ TokensReasoning: reasoningTokens,
```
```diff
+ // models_contract_test.go — таблично по провайдерам:
+ //   JSON-режим включается; FinishReason заполнен; out/reasoning не пересекаются.
```
**Проверка:** новые юнит-тесты; пересчёт `total_cost` задокументировать в PR.
⚠️ Перед фиксом **подтвердить семантику по докам каждого SDK** (формулировка из
[[Ревью исследования — лог верификации]]).

---

## PR-6 — настоящий JSON у Anthropic
Сейчас `GenerateJSON` у Anthropic игнорирует флаг (`models.go:163`).
```diff
  // anthropic generate(jsonMode=true)
+ if jsonMode {
+     // вариант A: tool-use со схемой и tool_choice
+     // вариант B: assistant-prefill "{" чтобы форсить JSON-старт
+ }
```
**Проверка:** прогон normalize на anthropic-профиле → доля валидного JSON растёт;
[[Finding — Value Object находки|extractJSON]] остаётся как страховка.

---

## Принципы всего плана
- **Каждый PR зелёный** (build + test + fixture dry-run) — инкрементально.
- **Поведение не меняется** в PR-1..4 (чистый рефактор); PR-5..6 меняют учёт/надёжность —
  с тестами и записью в changelog.
- **Не трогаем контракты пользователя:** формат артефактов, Markdown-персоны, CLI-флаги.

## Связи
- Целевая раскладка: [[Рефакторинг к DDD-пакетам]].
- Карта границ: [[Контекстная карта — Bounded Contexts]].
- Источник дефектов: [[Ревью исследования — лог верификации]], [[Провайдеры — таблица различий]].
