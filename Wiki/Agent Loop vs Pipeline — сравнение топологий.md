---
title: Agent Loop vs Pipeline — сравнение топологий
type: research
tags: [agent-loop, pipeline, dag, architecture, comparison, chebupelka]
source: chebupelka (github.com/alexey-goloburdin/chebupelka) vs ai-reviewer
updated: 2026-06-24
---

# Agent Loop vs Pipeline — сравнение топологий

> **Суть:** есть два фундаментальных способа оркестровать LLM-вызовы — **цикл**
> (agent loop) и **граф** (directed pipeline). chebupelka — первый, ai-reviewer — второй.
> Выбор топологии диктуется тем, известна ли структура задачи заранее.

---

## Архитектурный обзор

### chebupelka — Agent Loop

```mermaid
graph TD
    U([user_message]) --> A[agent_loop]
    A --> L[call_llm\nmessages]
    L -->|tool_calls present| T[call_tool\nbash]
    T --> M[messages.append\nresult]
    M --> L
    L -->|tool_calls == | F([✅ Finish])

    style U fill:#eff6ff,stroke:#3b82f6,color:#1d4ed8
    style A fill:#eef2ff,stroke:#6366f1,color:#4338ca
    style L fill:#f0f9ff,stroke:#0ea5e9,color:#0369a1
    style T fill:#faf5ff,stroke:#a855f7,color:#7e22ce
    style M fill:#f8fafc,stroke:#cbd5e1,color:#475569
    style F fill:#f0fdf4,stroke:#22c55e,color:#15803d
```

**Ключевые свойства:**
- Число итераций неизвестно заранее — модель сама решает когда остановиться
- Результат каждого инструмента попадает в `messages` и влияет на следующий шаг
- Backstop: `MAX_TURNS = 1000` защищает от бесконечного цикла

---

### ai-reviewer — Directed Pipeline (DAG)

```mermaid
graph TD
    R[runOne] --> S1
    S1["Stage 1 — Pre-run Explainers\n‖ goroutines"] --> S2
    S2["Stage 2 — Reviewers\n‖ goroutines"] --> S25
    S25["Stage 2.5 — ApplyWaivers\nLLM-судья"] --> S3
    S3["Stage 3 — AggregateFindings"] --> S4
    S4["Stage 4 — Post-run Explainers\n‖ goroutines"] --> REP
    REP([✅ report.md])

    style R fill:#eef2ff,stroke:#6366f1,color:#4338ca
    style S1 fill:#f0f9ff,stroke:#0ea5e9,color:#0369a1
    style S2 fill:#faf5ff,stroke:#a855f7,color:#7e22ce
    style S25 fill:#fff1f2,stroke:#f43f5e,color:#be123c
    style S3 fill:#f0fdf4,stroke:#22c55e,color:#15803d
    style S4 fill:#f0f9ff,stroke:#0ea5e9,color:#0369a1
    style REP fill:#f0fdf4,stroke:#22c55e,color:#15803d
```

**Ключевые свойства:**
- Каждая персона вызывает LLM ровно 1 раз — нет обратной связи между вызовами
- Параллелизм горутинами внутри стадий, но стадии строго последовательны
- Граф ацикличен (DAG) — нет петель, нет условных переходов между стадиями

---

## Разбор кода: где в ai-reviewer «живёт» оркестрация

### runOne() — главный оркестратор

```go
// main.go:63
func runOne(ctx context.Context, runConfig *RunConfig, s *RunSettings) {
    runResults := NewRunResults()

    sem := make(chan struct{}, s.Concurrency) // семафор параллелизма

    runPersonas(ctx, runConfig.PreRunToRun,    rc, rr, sem, "pre-run explainers") // Stage 1
    runPersonas(ctx, runConfig.ReviewersToRun, rc, rr, sem, "reviewers")          // Stage 2
    ApplyWaivers(ctx, runConfig, runResults)                                       // Stage 2.5
    AggregateFindings(ctx, rc.BalancedClient, runResults.AllFindings)             // Stage 3
    runPersonas(ctx, runConfig.PostRunToRun,   rc, rr, sem, "post-run explainers") // Stage 4
    generateReport(...)                                                            // Report
}
```

`runOne()` — линейная последовательность вызовов. Это и есть весь "цикл" — но он не цикл.

### runPersonas() — параллельный Fan-out внутри стадии

```go
// main.go:144
func runPersonas(ctx context.Context, personas []PersonaRun, ...) {
    var wg sync.WaitGroup
    for _, run := range personas {
        wg.Add(1)
        go func(run PersonaRun) {
            defer wg.Done()
            sem <- struct{}{}        // захват слота (ограничение параллелизма)
            defer func() { <-sem }()
            run.Execute(ctx, rc, rr)
        }(run)
    }
    wg.Wait()   // барьер: все персоны стадии завершились → переходим к следующей
}
```

Это горутинный fan-out с семафором. Все персоны одной стадии запускаются параллельно,
затем `wg.Wait()` — барьер перед следующей стадией.

---

## Сравнительная таблица

| Критерий | chebupelka (loop) | ai-reviewer (pipeline) |
|---|---|---|
| **Топология** | цикл (feedback loop) | DAG (без циклов) |
| **Структура задачи** | неизвестна заранее | фиксирована (ревью кода) |
| **Число LLM-вызовов** | N × (неизвестно) | N × персон (известно) |
| **Обратная связь** | да — результат → следующий prompt | нет — персоны независимы |
| **Параллелизм** | нет (1 агент) | горутины внутри стадий |
| **Стоп-условие** | отсутствие tool_calls | конец последней стадии |
| **Стоимость** | непредсказуема | детерминирована (RunPlan) |
| **Отказоустойчивость** | падает вся цепочка | skip + continue |

---

## Почему ai-reviewer выбрал DAG, а не loop

**Задача ревью кода структурирована.** Мы знаем:
1. какие файлы изменились (до запуска)
2. какие персоны нужны (конфиг)
3. какой порядок стадий обязателен (pre → review → waiver → aggregate → post)

Когда задача структурирована — loop избыточен: он добавляет непредсказуемость
стоимости и число вызовов, но не даёт выигрыша.

**Loop нужен, когда задача открытая:** «напиши скрипт», «отладь этот баг» —
количество шагов непредсказуемо, инструмент `bash` нужен на каждом шаге.

> **Правило выбора:** знаешь граф задачи → DAG pipeline. Не знаешь → agent loop.

---

## Единое ядро: одиночный LLM-вызов

Несмотря на разную топологию, **нижний уровень идентичен** — один HTTP-запрос к LLM:

```python
# chebupelka
def call_llm(messages):
    resp = requests.post(f"{LLM_BASE_URL}/chat/completions", json={
        "messages": messages, "tools": LLM_TOOLS, "tool_choice": "auto"
    })
    return content, tool_calls
```

```go
// ai-reviewer (Persona.Run → client.Generate)
result, err = client.Generate(personaCtx, prompt, maxTokens)
```

Разница только в том, что chebupelka передаёт `tools` и ожидает `tool_calls`,
а ai-reviewer ожидает текстовый ответ (или JSON через `GenerateJSON`).

---

## Связи
- Хаб: [[MOC — ai-reviewer]]
- Подробно о персоне как единице вызова: [[Persona — корень агрегата ревью]]
- Конвейер ai-reviewer пошагово: [[Sequence — конвейер ревью]]
- Исходник loop-паттерна: [[chebupelka — минимальный coding agent]]
- Агрегация как отдельный LLM-шаг: [[Finding — Value Object находки]]
