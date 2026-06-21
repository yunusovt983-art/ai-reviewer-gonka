---
title: Persona — корень агрегата ревью
type: concept
tags: [ai-reviewer, ddd, domain, persona]
source: persona.go:15
updated: 2026-06-20
---

# Persona — корень агрегата ревью

> **Суть:** персона — единица *намерения*, а не линтер. Это YAML-frontmatter +
> свободные инструкции в Markdown. Узкая персона отвечает на один вопрос хорошо
> (первопринцип специализации из [[MOC — ai-reviewer]]).

## Структура (`persona.go:15`)
```go
type Persona struct {
    ID                string
    ModelCategory     string    // логическая категория, не имя модели
    MaxTokens         *int      // указатель: nil ≠ 0 (не задано vs «без лимита»)
    Filters           FilterSet // inline во frontmatter
    Role              string    // "reviewer" (по умолч.) | "explainer"
    Stage             string    // "pre" | "post" (для explainer)
    IncludeFindings   bool      // подмешать находки предыдущих персон
    IncludeExplainers []string  // подмешать вывод конкретных pre-explainer'ов
    ExcludeDiff       bool      // в промпт идёт только статистика
    Instructions      string    // тело Markdown
}
```

## Конечный автомат «роль × стадия»
| Role | Stage | Когда | Вывод | Особенность |
|---|---|---|---|---|
| `explainer` | `pre` | до ревьюеров | строгий JSON `{files:[{file,analysis}]}` | **кэшируется** по SHA |
| `reviewer` | — | основная стадия | свободный текст → нормализация | производит [[Finding — Value Object находки|Finding]] |
| `explainer` | `post` | после агрегации | свободный Markdown | в финальный отчёт |

## Живой пример — самоприменение инструмента
`ai-reviewer-philosophy.md` проверяет **не баги, а соответствие философии продукта**:
```yaml
id: ai-reviewer-philosophy
model_category: balanced
path_filters: ["*.go", ".ai-review/gonka-ai/ai-reviewer/**", "README.md"]
---
You are the product philosophy reviewer for ai-reviewer.
Check ... low surprise and high operator trust;
repo-scoped configuration rather than spooky global behavior ...
```

## Связанные идеи
- **Каскад лимита токенов** через `*int`: CLI ▸ persona ▸ model config (`persona.go:104`).
  `nil` = не задано, `0` = явно «без лимита». Зафиксировано в `max_tokens_test.go`.
- **Кэш pre-explainer'ов** по `SHA256(instructions + headSHA)` (`persona.go:78`) —
  кэшируется только детерминированное.
- Каждая персона получает **суженный** [[PRContext — ревьюируемый мир|PRContext]] только
  из релевантных файлов — это решает [[FilterSet — управление стоимостью]].

## Связи
- Куда исполняется: [[Sequence — конвейер ревью]].
- Чем питается: [[Primer и Concept — инъекция знания]], [[Composition Root — NewRunConfig]].
- Что производит: [[Finding — Value Object находки]].
