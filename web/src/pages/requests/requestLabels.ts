import type { TFunction } from 'i18next'
import type { BookVariantStatus, LibraryRequest } from '../../api/client'

// Dispatched on window after an admin approves or declines, so the nav badge
// recounts pending requests without polling.
export const REQUESTS_CHANGED_EVENT = 'bindery:requests-changed'

// The one line status a request shows, shared by the requester's list and
// the admin queue so both say the same thing about the same row.
export function requestStatusLabel(t: TFunction, r: LibraryRequest): string {
  if (r.status === 'declined') return t('requests.status.declined')
  if (r.status === 'pending') return t('requests.status.pending')
  if (r.kind === 'author' && r.booksTotal) {
    return t('requests.status.authorProgress', { imported: r.booksImported ?? 0, total: r.booksTotal })
  }
  return r.fulfilled ? t('requests.status.available') : t('requests.status.approved')
}

export function requestStatusClass(r: LibraryRequest): string {
  if (r.status === 'declined') return 'bg-red-100 text-red-800 dark:bg-red-900/30 dark:text-red-300'
  if (r.status === 'pending') return 'bg-amber-100 text-amber-800 dark:bg-amber-900/30 dark:text-amber-300'
  return r.fulfilled
    ? 'bg-emerald-100 text-emerald-800 dark:bg-emerald-900/30 dark:text-emerald-300'
    : 'bg-sky-100 text-sky-800 dark:bg-sky-900/30 dark:text-sky-300'
}

export function mediaTypeLabel(t: TFunction, m: string): string {
  if (m === 'ebook') return t('common.ebook')
  if (m === 'audiobook') return t('common.audiobook')
  if (m === 'both') return t('common.both')
  return t('requests.anyFormat')
}

function requestFormatsFromMediaType(mediaType: string): Array<'ebook' | 'audiobook'> {
  if (mediaType === 'both') return ['ebook', 'audiobook']
  if (mediaType === 'audiobook') return ['audiobook']
  if (mediaType === 'ebook') return ['ebook']
  return []
}

function normalizeLanguage(language: string): string {
  return (language || '').trim().toLowerCase()
}

// Pending requests often arrive before their per-variant projection exists.
// Build a display-only matrix from mediaType + languages so the queue and the
// approval form can show exactly what the admin is about to approve.
export function pendingRequestVariants(request: LibraryRequest, overrideMediaType = ''): BookVariantStatus[] {
  if (request.kind !== 'book') return []
  const mediaType = overrideMediaType || request.mediaType
  const formats = requestFormatsFromMediaType(mediaType)
  if (formats.length === 0) return request.variants ?? []

  const selectedLanguages = Array.from(
    new Set((request.languages ?? []).map(normalizeLanguage).filter(Boolean)),
  )
  const languages = selectedLanguages.length > 0 ? selectedLanguages : ['']
  const existing = new Map((request.variants ?? []).map(v => [`${v.mediaType}/${normalizeLanguage(v.language)}`, v]))
  const rows: BookVariantStatus[] = []

  for (const format of formats) {
    for (const language of languages) {
      rows.push(existing.get(`${format}/${language}`) ?? {
        id: -(rows.length + 1),
        mediaType: format,
        language,
        status: 'wanted',
        verification: '',
      })
    }
  }
  return rows
}
