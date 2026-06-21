---
title: Git-дифф и pathspec — добыча изменений
type: process
tags: [ai-reviewer, ddd, git, diff, pathspec, cost]
source: context.go:498-742
updated: 2026-06-20
---

# Git-дифф и pathspec — добыча изменений

> **Суть:** механика, которая **наполняет** [[PRContext — ревьюируемый мир]] из git.
> `GetPRContext` (`context.go:526`) выбирает один из двух путей и применяет фильтры
> **на уровне git pathspec** ещё до парсинга — это нижний этаж экономики токенов.

## Развилка режимов (`context.go:533`)
```go
if BaseRefOid == HeadRefOid && !IsCommit && BaseRefOid != "" {
    // FILE-MODE
} else {
    // NORMAL diff-mode
}
```
Guard `BaseRefOid != ""` отсекает ложное срабатывание при пустых SHA.

## File-mode — целый файл как «новый» (`context.go:539-587`)
1. `GetFilesForPatterns` → `git ls-tree -r --name-only <SHA>` + `MatchesPath`.
2. По каждому файлу `git show <SHA>:<file>` → содержимое.
3. **Фабрикуется дифф:** `+++ b/<file>` + `@@ -0,0 +1,N @@` + каждая строка с `+`.
4. `AnnotateDiff` → `FileContext` → фильтр `Matches`.

## Normal-mode — реальный дифф (`context.go:590-624`)
1. `GetDiff(base, head)` (см. ниже).
2. `strings.Split(diff, "diff --git ")` → по-файловые куски (заголовок до первого
   файла отбрасывается).
3. По куску: `AnnotateDiff` → `ParseAnnotatedFileDiff` → фильтр.

Оба пути сходятся в одинаковый `[]FileContext` → **единый downstream** (идея «file-mode
как фейковый дифф», см. [[PRContext — ревьюируемый мир]]).

## `GetDiff` — triple-dot + pathspec (`context.go:712`)
```go
args := ["diff", "<base>...<head>"]   // triple-dot = от общего предка к head
// фильтры транслируются в git pathspec:
//   include → "<f>"
//   exclude → ":(exclude)<f>"
//   global  → ":(exclude)<f>"  ТОЛЬКО если не в include
```
- **Triple-dot** (`A...B`) безопаснее для PR-флоу: показывает только то, что добавил
  head относительно точки ветвления, игнорируя изменения базы.
- Глобальные исключения уходят **в сам git** → исключённые файлы вообще не выгружаются.
  Это и есть «убрать ~95% объёма до LLM», см. [[FilterSet — управление стоимостью]].

## `MatchesPath` и `pathIncluded` (`context.go:635, 672`)
- Рекурсия `Any`(OR)/`All`(AND); include с `defaultOnEmpty=true` (пусто = брать всё);
  exclude; global-excludes-unless-included.
- `pathIncluded` через `go-pathspec`, с **трюком:** обрезает ведущий `./` у паттернов
  (pathspec обрезает его у пути, но не у паттерна — иначе несовпадение).

## Ленивая компиляция (`Compile`, context.go:498)
Регэкспы (`RawRegexFilters`→`RegexFilters`, `IssueRegexes`→`IssueRegexObjects`)
компилируются **just-in-time** и **рекурсивно** в `Any`/`All`. Ошибка regex → ошибка
загрузки, не паника.

## `hunkHeaderRegexp` (`context.go:744`)
`^@@ -\d+(?:,\d+)? \+(\d+)(?:,\d+)? @@` — захватывает **стартовую строку новой версии**,
с неё `AnnotateDiff` ведёт счётчик. Основа точных номеров строк.

## Связи
- Что наполняет: [[PRContext — ревьюируемый мир]].
- Чем фильтрует: [[FilterSet — управление стоимостью]].
- Кто вызывает: [[Composition Root — NewRunConfig]] (фаза 5, глобальный контекст).
- Тест поведения: [[Fixture dry-run — тест фильтрации без токенов]].
