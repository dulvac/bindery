import { useTranslation } from 'react-i18next'
import type { BookVariantStatus } from '../api/client'
import { variantLabel } from '../util/bookVariant'

export default function ChooseBookVariant({ variants, value, onChange, disabled, pending }: {
  variants: BookVariantStatus[]
  value: number | null
  onChange: (id: number | null) => void
  disabled?: boolean
  pending?: boolean
}) {
  const { t } = useTranslation()
  const label = pending ? t('variants.choosePending', 'Wanted copy for pending grab') : t('variants.choose', 'Wanted copy for manual search')
  return (
    <label className="text-xs text-fg-muted flex flex-wrap items-center gap-2">
      <span>{label}</span>
      <select aria-label={label}
        value={value ?? ''} disabled={disabled}
        onChange={e => onChange(e.target.value ? Number(e.target.value) : null)}
        className="bg-slate-200 dark:bg-zinc-800 border border-slate-300 dark:border-zinc-700 rounded px-2 py-1">
        <option value="">{t('variants.select', 'Select a wanted copy')}</option>
        {variants.map(v => <option key={v.id} value={v.id}>{variantLabel(v, t)}</option>)}
      </select>
    </label>
  )
}
