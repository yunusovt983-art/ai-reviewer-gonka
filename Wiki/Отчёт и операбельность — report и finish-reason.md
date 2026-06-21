---
title: Отчёт и операбельность — report и finish-reason
type: process
tags: [ai-reviewer, report, operability, main, context-eval]
source: main.go:63-486
updated: 2026-06-20
---

# Отчёт и операбельность — report и finish-reason

> **Суть:** `main.go` — дирижёр (`runOne`) + сборка `report.md` (`generateReport`) +
> рендер [[Agent Handoff — скептический второй агент|handoff]]. Здесь живут операторские
> мелочи, повышающие доверие: предупреждение об обрезке вывода и embed-шаблон.

## Оркестрация `runOne` (`main.go:63`)
`SetDiffStats` → pre → reviewers → **[если PromptOnly: post + handoff + return]** →
`ApplyWaivers` → `all_findings.json` → `AggregateFindings(BalancedClient)` → `summary.md`
(StripMarkers) → лог агрегатора → post → `report.md` → `agent_handoff.md` → `stats.txt`.
Подробный поток: [[Sequence — конвейер ревью]].

## Идея — предупреждение по finish-reason (`main.go:345`)
В `report.md` каждая запись помечается **⚠️ Warning**, если `FinishReason` **не** входит в
{`STOP`,`stop`,`end_turn`,`FinishReasonStop`}:
```
⚠️ **Warning: MAX_TOKENS**
```
→ оператор сразу видит **обрезанный/ненадёжный** вывод персоны (упёрлись в лимит токенов
и т.п.). Дешёвый, но сильный сигнал доверия. `FinishReason` течёт из
[[Провайдеры — таблица различий|всех провайдеров]].

## Структура `report.md` (`generateReport`, main.go:304)
- Заголовок (PR/commit/files) + base/head SHA.
- Summary (через `LinkPersonas` → кликабельные `@persona{ID}`).
- `## Explanations` — вывод post-explainer'ов.
- `## Stats` — по персоне: In/Out/(Thinking)/Time/Cost/⚠️; **`### Usage by Model`**
  (агрегат по моделям); `### Estimated Total Cost`.
- `## Waived Issues` — подавленные находки с локацией (см. [[Waiver — LLM-судья подавления]]).

⚠️ Формула стоимости здесь (`main.go:341`) — **третья копия** (ещё в `settings.go:138`
и input-only в context-eval). Кандидат на вынос: [[PR-план рефакторинга — пошаговые diff'ы]].

## Идея — go:embed шаблона handoff (`main.go:19`)
```go
//go:embed agent_handoff.md.tmpl
var handoffTemplateSource string
var handoffTemplate = template.Must(...)
```
Шаблон **вшит в бинарь** → single-binary без внешних файлов. `generateAgentHandoff`
(`main.go:427`) наполняет `handoffData` (метаданные, дедуп праймеров, списки персон по
стадиям) и исполняет шаблон.

## Идея — context-eval считает стоимость только по input (`main.go:164`)
`runContextEval` сперва гоняет pre-explainer'ы (для точности), затем по каждой персоне
строит [[Конструктор промптов — порядок и бюджет|PromptBuilder.Build]] и печатает
токены по категориям. CSV-выгрузка с **типизированным заголовком** (`Integer/Float/
String/StringPath`, `main.go:267-268`) — готова для Datasette/анализа. Стоимость =
`tokens·inputPrice` (это **цена промпта**, не полного прогона).

## Связи
- Поток стадий: [[Sequence — конвейер ревью]] · Учёт: [[Учёт и отчётность — RunResults и stats]].
- Что рендерит: [[Agent Handoff — скептический второй агент]].
- Кандидаты на рефакторинг: [[PR-план рефакторинга — пошаговые diff'ы]].
