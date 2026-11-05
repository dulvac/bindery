import { useTranslation } from 'react-i18next'

const REQUEST_LANGUAGE_CODES = ['eng', 'fre', 'rum'] as const

type RequestLanguageCode = (typeof REQUEST_LANGUAGE_CODES)[number]

const LABELS: Record<RequestLanguageCode, string> = {
  eng: 'bookLanguages.english',
  fre: 'bookLanguages.french',
  rum: 'bookLanguages.romanian',
}

interface Props {
  value: string[]
  onChange: (value: string[]) => void
  disabled?: boolean
}

export default function BookLanguages({ value, onChange, disabled = false }: Props) {
  const { t } = useTranslation()
  const selected = new Set(value)

  const toggle = (language: RequestLanguageCode, checked: boolean) => {
    const next = checked ? [...value, language] : value.filter(code => code !== language)
    const unique = new Set(next)
    onChange(REQUEST_LANGUAGE_CODES.filter(code => unique.has(code)))
  }

  return (
    <fieldset className="space-y-2" disabled={disabled}>
      <legend className="text-sm font-medium">{t('bookLanguages.label')}</legend>
      <div className="flex flex-wrap gap-x-4 gap-y-2">
        {REQUEST_LANGUAGE_CODES.map(code => (
          <label key={code} className="flex items-center gap-2 text-sm select-none">
            <input
              type="checkbox"
              checked={selected.has(code)}
              onChange={event => toggle(code, event.target.checked)}
              className="rounded border-slate-400 dark:border-zinc-600"
            />
            <span>{t(LABELS[code])}</span>
          </label>
        ))}
      </div>
    </fieldset>
  )
}
