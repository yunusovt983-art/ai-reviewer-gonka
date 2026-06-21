---
title: Fixture dry-run — тест фильтрации без токенов
type: process
tags: [ai-reviewer, testing, ci, dry-run, fixtures]
source: scripts/check_fixture_dry_run.sh, .github/workflows/fixture-dry-run.yml
updated: 2026-06-20
---

# Fixture dry-run — тест фильтрации без токенов

> **Суть (умная идея тестирования):** самую важную для экономики механику —
> [[FilterSet — управление стоимостью|фильтрацию персон/праймеров]] — нельзя проверить
> юнит-тестом «модели». Решение: **fixture-репозиторий + `--dry-run`** = детерминированный
> интеграционный тест, который **не тратит ни одного токена**.

## Как устроено
- Отдельный репозиторий-фикстура `gonka-ai/ai-reviewer-fixture` с **выверенными PR**
  (#1, #2), каждый меняет файлы так, чтобы зацепить **конкретные типы фильтров**.
- В фикстуре — набор персон-под-фильтр: `branch-reviewer`, `date-reviewer`,
  `regex-reviewer`, `function-reviewer`, `line-window-reviewer`, `go-reviewer`,
  `docs-reviewer`, `frontend-reviewer`, `legacy-reviewer` (см. `.ai-review/.../fixture/personas/`).
- `check_fixture_dry_run.sh`: `go build` → `ai-reviewer pr … N --dry-run` → `strip_ansi`
  → `assert_contains` / `assert_not_contains` по выводу.

## Что именно проверяется (контракт)
1. **Какие персоны запустятся vs пропустятся** на данном PR:
   - PR#2 → запускаются все 9 ревьюеров.
   - PR#1 → запускаются только `docs/frontend/legacy`, остальные
     `To be skipped (no matching files)`. Это проверяет
     [[Composition Root — NewRunConfig|деление на 6 групп]].
2. **Какие праймеры совпали:** `with primer: api-guardrails (matches 1 files)` и
   `legacy-warning (matches 1 files)` → проверяет [[Primer и Concept — инъекция знания|матчинг праймеров]].
3. **Precedence обнаружения конфига:** ассерт, что грузится
   `.ai-review/gonka-ai/ai-reviewer-fixture/config.yaml`, а **НЕ**
   `.repos/.../.ai-review/config.yaml` → проверяет [[Обнаружение артефактов — 3 слоя]].

## CI (`fixture-dry-run.yml`)
Триггеры: push/PR при изменении `*.go`, `go.mod/sum`, скрипта или фикстуры.
Раннер `macos-latest`, права `contents: read` + `pull-requests: read`, `GH_TOKEN`.
→ любое изменение логики фильтрации/обнаружения **немедленно** ловится на реальном PR.

## Почему это сильный паттерн (переносимо)
- `--dry-run` делает дорогую LLM-систему **детерминированно тестируемой**.
- Тест на **поведении CLI** (вывод), а не на внутренних функциях → переживает рефакторинг
  (важно для [[Рефакторинг к DDD-пакетам]]).
- Фикстура версионирует «трудные кейсы» фильтров как реальные диффы.

## Связи
- Что тестирует: [[FilterSet — управление стоимостью]], [[Обнаружение артефактов — 3 слоя]].
- Чем включается: [[Composition Root — NewRunConfig]] (`--dry-run`, ранний выход без клиентов).
- Идея в списке: [[20 переносимых идей ai-reviewer]] (#19 наблюдаемость без затрат).
