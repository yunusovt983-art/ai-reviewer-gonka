---
title: Рефакторинг к DDD-пакетам
type: proposal
tags: [ai-reviewer, ddd, refactoring, go, proposal]
updated: 2026-06-20
status: proposal (не реализовано в upstream)
---

# Рефакторинг к DDD-пакетам ⭐

> **Суть:** сделать смысловые границы из [[Контекстная карта — Bounded Contexts]]
> *физическими* Go-пакетами. Сегодня всё в плоском `package main` (`*.go` в корне),
> и `NewRunConfig` совмещает wiring зависимостей с бизнес-оркестрацией. Цель —
> domain без импортов инфраструктуры (правило зависимостей внутрь).

## Целевая раскладка модулей

```
ai-reviewer/
├── cmd/
│   └── ai-reviewer/
│       └── main.go                  # только парсинг argv → app.Run(); тонкий
├── internal/
│   ├── domain/                      # ⚙️ REVIEW DOMAIN — БЕЗ импортов infra
│   │   ├── review/
│   │   │   ├── persona.go           # Persona (агрегат), роли/стадии
│   │   │   ├── finding.go           # Finding (VO), severity, confidence
│   │   │   ├── primer.go            # Primer + Concept
│   │   │   ├── waiver.go            # Waiver (Policy)
│   │   │   └── pipeline.go          # доменные шаги: normalize/aggregate (чистые)
│   │   └── codebase/                # 🌍 CODE CONTEXT (доменная часть)
│   │       ├── prcontext.go         # PRInfo, PRContext, FileContext (Entity/VO)
│   │       ├── filterset.go         # FilterSet (VO предиката) + Matches()
│   │       └── diff.go              # AnnotateDiff, ParseAnnotatedFileDiff (чистые)
│   ├── app/                         # 🎭 APPLICATION — оркестрация
│   │   ├── run.go                   # RunService.Execute() — 7 стадий конвейера
│   │   ├── plan.go                  # планирование: персоны → {Run,Skip} группы
│   │   └── config_root.go           # composition root (бывш. NewRunConfig)
│   └── infra/                       # инфраструктура (реализует port'ы домена)
│       ├── modelclient/             # 🔌 ACL: openai.go anthropic.go gemini.go
│       ├── gitsource/               # git.go + GitHub (gh) адаптер
│       ├── policystore/             # scanner.go — обнаружение артефактов
│       ├── configstore/             # config.go — профили моделей
│       └── artifacts/               # OutputHandler, run-log.jsonl
└── go.mod
```

## Правило зависимостей (главное)
```
cmd → app → domain ← infra
                ▲
         (infra реализует интерфейсы-port'ы, объявленные в domain)
```
- `domain` **ничего** не импортирует из `infra`/`app`. Чистые функции и типы.
- `infra` зависит от `domain` (реализует его интерфейсы).
- `app` склеивает: знает и `domain`, и `infra`-конструкторы.

## Шаг 1 — вынести port'ы (инверсия зависимостей)
В домене объявить интерфейсы, которые сейчас «торчат» в infra:

```go
// internal/domain/review/ports.go
package review

type ModelClient interface {                  // было в models.go
    Generate(ctx context.Context, prompt string, maxTokens int) (ModelResult, error)
    GenerateJSON(ctx context.Context, prompt string, maxTokens int) (ModelResult, error)
}

type PolicyStore interface {                  // было scanner.go
    Personas() []Persona
    Primers()  []Primer
    Waivers()  []Waiver
}
```
`modelclient` (infra) теперь **реализует** `review.ModelClient`. Это превращает
[[Model Access — ACL над провайдерами]] в честный port/adapter.

## Шаг 2 — разбить `NewRunConfig` (god-method)
Сегодня [[Composition Root — NewRunConfig|NewRunConfig]] делает 7 вещей сразу. Разделить:

| Новый объект | Что забирает из NewRunConfig |
|---|---|
| `infra` конструкторы | EnsureRepo, FetchRefs, GetPRInfo, загрузка артефактов/конфига |
| `app.Planner` | фильтрация персон в 6 групп, расчёт суженных `PRContext` |
| `app.RunService` | порядок стадий, конкурентность, агрегация результатов |
| `cmd/main` | разбор argv → сборка `RunService` через DI |

Composition root остаётся (он нужен), но становится **тонким**: только `new`-и и
проброс, без бизнес-логики планирования.

## Шаг 3 — централизовать жизненный цикл клиентов
Сейчас normalize/aggregate шарят клиентов, а персоны/вейверы создают на лету.
Ввести `modelclient.Pool`, который кэширует клиент по `(provider, model, reasoning)`
и отдаётся всем стадиям. Убирает дублирование и упрощает учёт стоимости.

## Что НЕ трогать (осознанно)
- **Артефакты как Markdown** — формат стабилен, это контракт с пользователями.
- **Промпт-driven инварианты** — нормально для LLM-систем; компенсируется
  [[20 переносимых идей ai-reviewer|тестами и артефактами]].
- **Плоскую модель данных Finding/Persona** — она уже хороша; меняем только *пакеты*.

## Порядок внедрения (низкий риск → высокий)
1. Выделить `domain/codebase` (чистые `diff.go`, `filterset.go`) — нет внешних зависимостей.
2. Выделить `domain/review` + port'ы; infra реализует интерфейсы.
3. Распилить `NewRunConfig` на `infra`-конструкторы + `app.Planner` + `app.RunService`.
4. `modelclient.Pool` и финальная чистка `cmd/main`.

Каждый шаг компилируется и проходит существующие тесты — рефакторинг инкрементальный.

## Связи
- Карта границ: [[Контекстная карта — Bounded Contexts]].
- Что выносим в port: [[Model Access — ACL над провайдерами]].
- Что распиливаем: [[Composition Root — NewRunConfig]].
