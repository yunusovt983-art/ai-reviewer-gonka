---
title: Контекстная карта — Bounded Contexts
type: architecture
tags: [ai-reviewer, ddd, context-map, diagram]
updated: 2026-06-20
---

# Контекстная карта — Bounded Contexts

> **Суть:** физически код — плоский пакет `main`, но по ответственности он распадается
> на 5 чётких **bounded contexts**. Это карта *смысловых* границ, которую
> [[Рефакторинг к DDD-пакетам]] предлагает сделать физической.

## Архитектурный обзор

```mermaid
graph LR
    subgraph DOMAIN["Domain Layer"]
        CB["Codebase BC<br/>PRInfo · FilterSet<br/>PRContext · LineRange"]
        RV["Review BC<br/>Finding · ModelClient port<br/>NormalizePersonaOutput"]
        PL["Policy BC<br/>Waiver · WaiverEvaluation"]
        KN["Knowledge BC<br/>Primer · Concept"]
    end
    subgraph INFRA["Infrastructure"]
        MA["Model Access ACL<br/>OpenAIClient · AnthropicClient · GeminiClient"]
        GIT["Git / VCS<br/>GetPRInfo · GetDiff · FetchRefs"]
    end
    subgraph APP["Application"]
        RC["RunConfig · RunPlan<br/>ClientPool · planRuns()"]
    end
    APP --> DOMAIN
    APP --> INFRA
    INFRA -->|implements ModelClient port| RV
    CB --> RV
    KN --> RV
    PL --> RV
```

## Диаграмма (mermaid)

```mermaid
flowchart TB
    subgraph APP["🎭 APPLICATION / ORCHESTRATION"]
        main["main.go — порядок стадий, конкурентность"]
        settings["settings.go — NewRunConfig (composition root)"]
    end

    subgraph POLICY["📜 REVIEW POLICY (артефакты)"]
        scanner["scanner.go — обнаружение, дедуп"]
        config["config.go — профили моделей"]
    end

    subgraph DOMAIN["⚙️ REVIEW DOMAIN (ядро)"]
        persona["persona.go"]
        pipeline["pipeline.go — Finding, normalize, aggregate"]
        primer["primer.go"]
        waiver["waiver.go"]
    end

    subgraph CTX["🌍 CODE CONTEXT (ревьюируемый мир)"]
        context["context.go — PRContext, FilterSet, diff"]
        git["git.go — fetch refs"]
    end

    subgraph ACL["🔌 MODEL ACCESS (ACL)"]
        models["models.go — ModelClient: OpenAI/Anthropic/Gemini"]
    end

    APP -->|планирует прогон| POLICY
    APP -->|дирижирует| DOMAIN
    POLICY -->|кормит персонами| DOMAIN
    CTX -->|даёт контекст| DOMAIN
    DOMAIN -->|вызывает модели| ACL
    APP -->|разрешает цель| CTX
```

## Роли контекстов (кратко)

| Контекст | Ответственность | Ключевая заметка |
|---|---|---|
| **Review Domain** | что есть находка, как персона исполняется, normalize/waive/aggregate | [[Persona — корень агрегата ревью]] |
| **Code Context** | строит неизменяемый «мир изменений» из git | [[PRContext — ревьюируемый мир]] |
| **Review Policy** | обнаружение/загрузка артефактов и конфига | [[Обнаружение артефактов — 3 слоя]] |
| **Model Access** | изолирует провайдеров за `ModelClient` | [[Model Access — ACL над провайдерами]] |
| **Application** | CLI, планирование, оркестрация | [[Composition Root — NewRunConfig]] |

## Почему это важно (DDD)
Границы контекстов = границы, по которым язык **не должен** протекать. `Finding.Source`
ссылается на `Persona.ID`, но домен ничего не знает про API Anthropic — это держит
[[Model Access — ACL над провайдерами|ACL]]. Нарушение этих границ — главный архитектурный
долг (см. раздел «слабые стороны» в `ARCHITECTURE-DDD.md`).

## Связи
- Следующий шаг: [[Sequence — конвейер ревью]] — как контексты взаимодействуют во времени.
- Цель: [[Рефакторинг к DDD-пакетам]] — сделать эти границы физическими пакетами.
