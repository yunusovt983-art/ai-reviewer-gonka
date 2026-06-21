---
title: PRContext — ревьюируемый мир
type: concept
tags: [ai-reviewer, ddd, domain, context, diff]
source: context.go:17
updated: 2026-06-20
---

# PRContext — ревьюируемый мир

> **Суть:** Code Context строит из git/GitHub **неизменяемое представление изменений**:
> `PRInfo` → `PRContext` → `[]FileContext`. Это «мир», который видят персоны.

## PRInfo — дескриптор цели (`context.go:17`)
```go
type PRInfo struct {
    Title, Body                 string
    BaseRefName, BaseRefOid     string  // база (SHA)
    HeadRefName, HeadRefOid     string  // голова (SHA)
    IsCommit                    bool
    CommitDate                  time.Time
    FilePatterns                []string
}
```
**Хитрость:** режим цели кодируется *комбинацией полей*, не enum-ом:
- `IsCommit=true` → коммит против родителя;
- `BaseRefOid == HeadRefOid && !IsCommit` → **file-mode**.

## FileContext — единица ревью (`context.go:37`)
```go
type FileContext struct {
    Filename     string
    Diff         string   // аннотированный: "LINE_NO:±content"
    ChangedLines []string // содержимое ±-строк
    Functions    []string // эвристически извлечённые имена
}
```

## Идея 1 — аннотированный дифф (`AnnotateDiff`, `context.go:746`)
Переписывает unified diff в `НОМЕР_СТРОКИ:±содержимое`, ведя счётчик через все ханки
(`+` инкрементит, `-` нет, контекст ` ` инкрементит). → LLM получает **точные номера
строк без галлюцинаций** и появляется возможность фильтра по диапазонам строк.

## Идея 2 — извлечение функций одним регэкспом (`context.go:751`)
```
(?:func|function|class|def|method|type)\s+([a-zA-Z_][a-zA-Z0-9_]*)
```
Грубо, языко-нейтрально, дёшево — питает `function_filters`.

## Идея 3 — file-mode через фейковый дифф (`context.go:554`)
Чтобы ревьюить целые файлы, фабрикуется дифф `@@ -0,0 +1,N @@` со всеми строками как
добавленными. **Один код-путь** работает и для диффов, и для целых файлов.

## Git-инфраструктура
`git.go`: `EnsureRepo`, `FetchRefs` (с фолбэком на `origin`/`FETCH_HEAD`), `GetDiff`
использует **triple-dot** `base...head` (общий предок → head), безопаснее для PR-флоу.
Полная механика добычи диффа/pathspec/file-mode: [[Git-дифф и pathspec — добыча изменений]].

## Связи
- Кто сужает этот мир: [[FilterSet — управление стоимостью]] (`FileContext.Matches`).
- Кто строит при планировании: [[Composition Root — NewRunConfig]].
- Кто потребляет: [[Persona — корень агрегата ревью]].
