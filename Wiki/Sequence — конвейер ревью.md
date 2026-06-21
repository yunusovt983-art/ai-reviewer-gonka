---
title: Sequence — конвейер ревью
type: architecture
tags: [ai-reviewer, ddd, pipeline, diagram, sequence]
updated: 2026-06-20
---

# Sequence — конвейер ревью

> **Суть:** ревью — это **упорядоченный** 7-стадийный конвейер (зависимости по данным),
> но **внутри стадии** персоны исполняются конкурентно (семафор на `Concurrency`,
> по умолчанию 5, `main.go:144`). Это и есть «оркестрация» из [[MOC — ai-reviewer|девиза]].

## Архитектурный обзор

```mermaid
sequenceDiagram
    participant CLI
    participant App as RunConfig
    participant Plan as planRuns()
    participant Pipeline
    participant LLM as ModelClient (port)

    CLI->>App: NewRunConfig(ctx, settings)
    App->>Plan: planRuns(personas, prInfo, ctx)
    Plan-->>App: RunPlan{PreRunToRun, ReviewersToRun...}

    loop Pre-explainers (параллельно)
        App->>LLM: GenerateJSON(ctx, prompt, maxTokens)
        LLM-->>App: ModelResult{Text, TokensIn, TokensOut}
    end

    loop Reviewers (параллельно, N персон)
        App->>LLM: Generate(ctx, prompt, maxTokens)
        LLM-->>App: ModelResult
        App->>LLM: GenerateJSON (normalize via FastestClient)
        LLM-->>App: []Finding
    end

    App->>LLM: ApplyWaivers (LLM-judge)
    App->>LLM: AggregateFindings via BalancedClient
    LLM-->>App: Markdown report
    App->>CLI: RunResults{AllFindings, Stats, Report}
```

## Стадии (горизонтально)

```
 ①              ②             ③           ④           ⑤            ⑥           ⑦
pre-       ─▶ reviewers  ─▶ normalize ─▶ waivers  ─▶ aggregate ─▶ post-    ─▶ report
explainers    (concurrent)  (cheap LLM)  (LLM-судья)  (balanced)   explainers
JSON,кэш SHA  raw text      Finding[]    -waived      Markdown     Markdown   stdout+
                                                      summary                 артефакты
```

## Sequence (mermaid)

```mermaid
sequenceDiagram
    participant App as main.go
    participant Pre as Pre-Explainers
    participant Rev as Reviewers
    participant Norm as Normalizer (cheap)
    participant Wav as Waivers (LLM-судья)
    participant Agg as Aggregator (balanced)
    participant Post as Post-Explainers
    participant RR as RunResults (mutex)

    App->>Pre: ① исполнить (concurrent)
    Pre-->>RR: PreRunAnalyses (map file→текст), кэш по SHA
    App->>Rev: ② исполнить (concurrent, +PreRunAnalyses в промпт)
    Rev-->>Norm: ③ сырой текст
    Norm-->>RR: Finding[] (строгий JSON)
    App->>Wav: ④ для каждой находки: фильтр + LLM
    Wav-->>RR: убрать waived → WaivedFindings (с [Waived by..])
    App->>Agg: ⑤ AllFindings
    Agg-->>RR: summary.md (dedup, кластеры, @persona{ID})
    App->>Post: ⑥ исполнить (видят summary)
    Post-->>RR: PostRunOutputs
    App->>App: ⑦ report.md + run-log.jsonl + stats
```

## Поток данных (через `RunResults`, защищён мьютексами)
1. **① → ②**: `PreRunAnalyses` подмешивается выборочно (`IncludeExplainers`).
2. **② → ③ → ⑤**: сырой текст → `[]Finding` → `AllFindings`. См. [[Finding — Value Object находки]].
3. **④**: [[Waiver — LLM-судья подавления|вейверы]] переносят подавленное в `WaivedFindings`.
4. **⑤**: [[Finding — Value Object находки|агрегация]] сохраняет атрибуцию `@persona{ID}`.

## Ключевые идеи стадий
- **Разная «дороговизна модели» по стадиям** → токены по ценности шага. Поиск — категория
  персоны; normalize — дёшево; waivers — `fastest_good`; aggregate — `balanced`.
  См. [[Model Category и Profile — позднее связывание]].
- **③ Сырой текст → отдельная нормализация** разделяет «поиск» и «структуризацию».
  Подробно: [[Finding — Value Object находки]].
- **Устойчивость к сбоям**: падение персоны логируется, но **не останавливает** конвейер
  (`main.go:156`); ошибка агрегации → дефолтный summary. Отчёт выдаётся всегда.

## Связи
- Кто что исполняет: [[Контекстная карта — Bounded Contexts]].
- Точка сборки: [[Composition Root — NewRunConfig]] делит персоны на 6 групп
  `{Pre,Reviewers,Post}{ToRun,ToSkip}` до запуска.
