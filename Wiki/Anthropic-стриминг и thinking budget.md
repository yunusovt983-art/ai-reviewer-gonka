---
title: Anthropic-стриминг и thinking budget
type: concept
tags: [ai-reviewer, ddd, models, anthropic, streaming, gotcha]
source: models.go:143-253
updated: 2026-06-20
---

# Anthropic-стриминг и thinking budget

> **Суть:** конкретная реализация одного провайдера за интерфейсом
> [[Model Access — ACL над провайдерами|ModelClient]]. Показывает, как `reasoning_level`
> превращается в **thinking budget**, и содержит **два подвоха**, которые стоит знать.

## Расчёт thinking budget (`models.go:192-215`)
Из `reasoning_level` (не `none`) и `MaxTokens`:
```
budget = 2048 (по умолчанию)
если MaxTokens > 4096        → budget = MaxTokens / 2
иначе если MaxTokens <= 2048 → budget = 1024 (если MaxTokens>1024) иначе 0
                               # band 2048<MaxTokens<=4096 оставляет budget=2048
если budget >= 1024:
    включить thinking(budget)
    если MaxTokens <= budget → MaxTokens = budget + 1024   # API требует max>budget
```
- Минимум для thinking — **1024**; при слишком малом `MaxTokens` thinking **молча
  отключается** (budget=0).
- `MaxTokens == 0` → принудительно **65536** (Anthropic требует max_tokens, `models.go:184`).

## Стриминг и учёт токенов (`models.go:217-238`)
`Messages.NewStreaming`, события:
| Событие | Что берём |
|---|---|
| `message_start` | `InputTokens`, `OutputTokens` |
| `content_block_delta`/`text_delta` | аккумулируем `text` |
| `message_delta` | финальные `OutputTokens`, `StopReason` |
`modelDisplay` = `model(reasoningLevel)`; суффикс `(...)` потом срезается в
[[Конструктор промптов — порядок и бюджет|tiktoken CountTokens]] — согласовано.

## ⚠️ Подвох 1 — reasoning-токены не видны (пробел наблюдаемости)
`tokensReasoning := 0` и **никогда не обновляется** из стрима (`models.go:218,248`).
Важный нюанс семантики: у Anthropic thinking-токены **тарифицируются внутри
`output_tokens`** — то есть в `TokensOut` они уже учтены, и `total_cost` скорее всего
**корректен**. Реальная проблема — **наблюдаемость**: в `report.md` нет строки
`Thinking: N` для Anthropic, не видно, сколько ушло на размышление.
Контраст по семантике токенов между провайдерами: [[Провайдеры — таблица различий]]
(там же — потенциальный **задвой** reasoning у OpenAI). Это аргумент за единый,
явно специфицированный контракт `ModelResult` — см.
[[PR-план рефакторинга — пошаговые diff'ы]].

## ⚠️ Подвох 2 — GenerateJSON у Anthropic не форсит JSON
`GenerateJSON` просто зовёт `generate(..., jsonMode=true)`, но **флаг фактически
игнорируется** (`models.go:163-171`): нет ни tool-use, ни `response_format`. JSON
держится **только на тексте промпта** + терпимости [[Finding — Value Object находки|extractJSON]].
Для нормализации/pre-explainer на Anthropic это структурная хрупкость (для OpenAI/Gemini
JSON-режим включается по-настоящему).

## Вывод для рефакторинга
Оба подвоха — аргумент за честный **port** `ModelClient` с контрактными тестами на
каждый провайдер (см. [[Рефакторинг к DDD-пакетам]]): расхождения провайдеров сейчас
скрыты внутри [[Model Access — ACL над провайдерами|ACL]].

## Связи
- Родитель: [[Model Access — ACL над провайдерами]].
- Что задаёт reasoning: [[Model Category и Profile — позднее связывание]].
- Куда течёт (недо)учёт: [[Учёт и отчётность — RunResults и stats]].
