import { useTranslation } from 'react-i18next'
import type { BookVariantStatus, LegacyBookFileStatus, MediaType } from '../api/client'
import { languageName } from '../util/language'
import MediaBadge from './MediaBadge'

interface Props {
  variants?: BookVariantStatus[]
  legacyFiles?: LegacyBookFileStatus[]
  className?: string
  testId?: string
  testIdPrefix?: string
}

type Row = {
  kind: 'variant' | 'legacy'
  key: string
  mediaType: MediaType
  language: string
  status: string
  verification: string
  legacyIndex?: number
}

const REQUEST_LANGUAGE_LABELS: Record<string, string> = {
  eng: 'bookLanguages.english',
  fre: 'bookLanguages.french',
  rum: 'bookLanguages.romanian',
}

function normalizeMediaType(mediaType: string): MediaType {
  return mediaType === 'audiobook' ? 'audiobook' : 'ebook'
}

function mediaOrder(mediaType: MediaType): number {
  return mediaType === 'ebook' ? 0 : 1
}

function sortRows(rows: Row[]): Row[] {
  return [...rows].sort((a, b) => {
    const mediaDiff = mediaOrder(a.mediaType) - mediaOrder(b.mediaType)
    if (mediaDiff !== 0) return mediaDiff
    return a.language.localeCompare(b.language)
  })
}

export default function BookVariantStatus({
  variants = [],
  legacyFiles = [],
  className,
  testId = 'book-variant-statuses',
  testIdPrefix = 'book-variant-status',
}: Props) {
  const { t } = useTranslation()

  const rows = sortRows([
    ...variants.map(v => ({
      kind: 'variant' as const,
      key: `variant-${v.id}`,
      mediaType: normalizeMediaType(v.mediaType),
      language: v.language,
      status: v.status,
      verification: v.verification,
    })),
    ...legacyFiles.map((f, legacyIndex) => ({
      kind: 'legacy' as const,
      key: `legacy-${legacyIndex}-${f.mediaType}`,
      mediaType: normalizeMediaType(f.mediaType),
      language: f.language,
      status: f.status,
      verification: f.verification,
      legacyIndex,
    })),
  ])

  if (rows.length === 0) return null

  const languageLabel = (language: string) => {
    const code = (language || '').trim().toLowerCase()
    if (!code) return t('bookDetail.languageUnknown')
    const requestLabel = REQUEST_LANGUAGE_LABELS[code]
    if (requestLabel) return t(requestLabel)
    return languageName(code) ?? code
  }

  const statusLabel = (status: string) => {
    switch ((status || '').trim().toLowerCase()) {
      case 'wanted':
        return t('bookStatus.wanted')
      case 'downloading':
        return t('queue.status.downloading')
      case 'imported':
        return t('bookStatus.imported')
      case 'review':
        return t('requests.variant.review', 'Review')
      default:
        return status
    }
  }

  const verificationLabel = (verification: string) => {
    const value = (verification || '').trim().toLowerCase()
    if (value === 'verified') return t('requests.variant.verified', 'Verified')
    if (value === 'unverified') return t('requests.variant.unverified', 'Unverified')
    return ''
  }

  const rowTestId = (row: Row) =>
    row.kind === 'legacy'
      ? `${testIdPrefix}-legacy-${row.legacyIndex ?? 0}`
      : `${testIdPrefix}-variant-${row.key.replace('variant-', '')}`

  return (
    <ul data-testid={testId} className={className ? `space-y-1 ${className}` : 'space-y-1'}>
      {rows.map(row => {
        const verification = verificationLabel(row.verification)
        return (
          <li
            key={row.key}
            data-testid={rowTestId(row)}
            className="text-xs text-fg-muted flex flex-wrap items-center gap-1.5"
          >
            <MediaBadge type={row.mediaType} />
            <span>{languageLabel(row.language)}</span>
            <span aria-hidden>·</span>
            <span>{statusLabel(row.status)}</span>
            {verification && (
              <>
                <span aria-hidden>·</span>
                <span>{verification}</span>
              </>
            )}
          </li>
        )
      })}
    </ul>
  )
}
