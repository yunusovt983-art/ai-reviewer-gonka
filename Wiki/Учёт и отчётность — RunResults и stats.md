---
title: Учёт и отчётность — RunResults и stats
type: concept
tags: [ai-reviewer, ddd, concurrency, accounting, output]
source: settings.go:17-213
updated: 2026-06-20
---

# Учёт и отчётность — RunResults и stats

> **Суть:** `RunResults` (`settings.go:32`) — потокобезопасный аккумулятор результатов
> прогона, в который конкурентные [[Persona — корень агрегата ревью|персоны]] пишут
> через **4 раздельных мьютекса**. `OutputHandler` — единая точка записи артефактов.

## Потокобезопасность — мелкозернистые мьютексы
```go
type RunResults struct {
    Stats          []RunLogEntry
    AllFindings    []Finding
    WaivedFindings []Finding
    PostRunOutputs []string
    PreRunAnalyses map[string][]string   // карта → особенно нуждается в защите
    ...
    statsMu, findingsMu, postRunOutputsMu, preRunAnalysesMu sync.Mutex
}
```
**Идея:** не один большой лок, а **по мьютексу на ресурс** (`AddStat`/`AddFindings`/
`AddPostRunOutput`/`AddPreRunAnalysis`, `settings.go:61-83`). Персоны разных стадий
пишут в разные поля → меньше contention. Это и есть единственная конкурентная зона
(см. [[Sequence — конвейер ревью]]).

## stats.txt — машиночитаемый итог (`GetStatsString`, settings.go:108)
Формат `key=value`:
```
lines_added / lines_removed / lines_changed
issues_<severity>=N         # подсчёт по Finding.SeverityHint, "" → unknown
tokens_in / tokens_out / tokens_reasoning
total_cost=%.6f
```
- `SetDiffStats` (`settings.go:89`) считает +/- строки из **аннотированного** диффа
  (пропуская `+++ `/`--- `). Связь: [[PRContext — ревьюируемый мир]].
- Стоимость: `in·inPrice/1e6 + (out+reasoning)·outPrice/1e6` — reasoning по выходной цене.
  ⚠️ **Та же формула продублирована** в `main.go:341` → кандидат на вынос при
  [[Рефакторинг к DDD-пакетам]].

## run-log.jsonl — кумулятивный аудит (`LogRun`, settings.go:176)
Одна `RunLogEntry` (JSON) на строку, **append** в `LogDir/run-log.jsonl`. На каждого
ревьюера — **две** записи: сама персона + `normalization:<id>` (см.
[[Finding — Value Object находки]]).

## Идея — один маркер, много рендереров (`settings.go:188-205`)
Агрегатор ставит `@persona{ID}`. `OutputHandler` рендерит его по каналу:
| Метод | Канал | Результат |
|---|---|---|
| `MarkPersona` | генерация | `@persona{id}` |
| `LinkPersonas` | Markdown-файл | `[@persona{id}](id/raw.md)` (клик в raw) |
| `Highlight` | терминал | ANSI-зелёный |
| `StripMarkers` | plain | просто `id` |
Чистое разделение «семантика маркера» vs «представление» — атрибуция персон
доезжает до [[Agent Handoff — скептический второй агент|handoff]] в нужном виде.

## Связи
- Куда пишут: [[Sequence — конвейер ревью]] (все стадии).
- Кто создаёт `OutputHandler`/`RunResults`: [[Composition Root — NewRunConfig]].
- Полный список артефактов: [[20 переносимых идей ai-reviewer]] (#20 observability).
