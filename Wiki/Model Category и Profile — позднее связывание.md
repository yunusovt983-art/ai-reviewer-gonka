---
title: Model Category и Profile — позднее связывание
type: concept
tags: [ai-reviewer, ddd, config, model, late-binding]
source: config.go, models.go:61
updated: 2026-06-20
---

# Model Category и Profile — позднее связывание

> **Суть:** персоны ссылаются на **логические категории** моделей, а не на имена.
> Профиль подменяет конкретные модели целиком. Сменить весь стек = сменить один флаг.

## Категории (`models.go:61`)
```go
const (
    FastestGood  = "fastest_good"  // дёшево/быстро (нормализация, вейверы)
    Balanced     = "balanced"      // агрегация
    BestCode     = "best_code"     // фолбэк агрегации
    FrontierBest = "frontier_best" // глубокий анализ
)
```
(в реальном `config.yaml` есть ещё `cheap`.)

## Двухуровневый конфиг (`config.go`)
- `model_definitions` — **переиспользуемые** описания моделей (provider, model, цены,
  `reasoning_level`).
- `model_profiles` — профиль → категория → модель (по `id` ссылается на definition,
  может перекрывать поля inline).

### Пример (реальный `ai-reviewer/config.yaml`)
```yaml
default_profile: gemini_standard
model_profiles:
  gemini_standard:
    balanced:      { id: gemini_2.5_flash }
    best_code:     { id: gemini_3.1_pro_preview, reasoning_level: low }
    frontier_best: { id: gemini_3.1_pro_preview, reasoning_level: high }
  anthropic:
    balanced:      { id: anthropic_haiku_4.5 }
    best_code:     { id: anthropic_sonnet_4.6, reasoning_level: low }
    frontier_best: { id: anthropic_opus_4.6 }
```

## Почему это «позднее связывание»
`--model-profile anthropic` мгновенно переключает **весь** стек моделей прогона на
Anthropic — персоны и промпты не меняются. Фолбэк-цепочка профиля:
CLI ▸ `default_profile` ▸ `"default"` ▸ первый доступный.

## `global_instructions`
Добавляются во все промпты, напр.: *«A correct report with no findings is better than a
noisy one»* — общая политика сигнал-над-шумом.

## Связи
- Кто транслирует `reasoning_level` в провайдеров: [[Model Access — ACL над провайдерами]].
- Кто использует категории: [[Persona — корень агрегата ревью]], [[Waiver — LLM-судья подавления]],
  [[Finding — Value Object находки|нормализация/агрегация]].
- Кто грузит: [[Обнаружение артефактов — 3 слоя]].
