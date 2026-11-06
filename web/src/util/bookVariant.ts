import type { TFunction } from 'i18next'
import type { Book, BookVariantStatus, SearchResult } from '../api/client'
import { canonicalLanguage, languageName } from './language'

// The Wanted list includes negative, display-only IDs for pending requests.
// Only the single-book detail response supplies IDs suitable for actions.
export function wantedCopies(book: Book): BookVariantStatus[] {
  return (book.variants ?? []).filter(v => Number.isSafeInteger(v.id) && v.id > 0 && v.status === 'wanted'
    && (v.mediaType === 'ebook' || v.mediaType === 'audiobook')
    && (book.mediaType === 'both' || book.mediaType === v.mediaType))
}

export function variantLabel(v: BookVariantStatus, t: TFunction): string {
  const format = t(`common.${v.mediaType}`, v.mediaType)
  const language = v.language === 'rum' ? t('bookLanguages.romanian', 'Romanian')
    : v.language === 'eng' ? t('bookLanguages.english', 'English')
      : v.language === 'fre' ? t('bookLanguages.french', 'French') : languageName(v.language) ?? v.language
  return `${format} · ${language}`
}

// The server owns the full release-language parser and makes the final grab
// decision. These MAM markers give the browser a conservative warning before
// asking a named admin for the override; a blank tag is never called English.
const mamMarker = /\[([a-z]{2,3})\s*\/\s*(?:EPUB|AZW3|MOBI|PDF|M4B|MP3|FLAC)\]/gi
// A conservative subset of the server's known language codes. Unrecognized
// bracket text (for example WEB, a release source) is not language evidence.
const knownMarkerCodes = new Set([
  'eng', 'fre', 'ger', 'dut', 'spa', 'ita', 'por', 'jpn', 'chi', 'rus',
  'kor', 'ara', 'swe', 'nor', 'dan', 'pol', 'cze', 'tur', 'hin', 'rum',
])
export function releaseMarkerLanguages(title: string): string[] {
  const codes = [...title.matchAll(mamMarker)].map(m => {
    const code = m[1].toLowerCase()
    return canonicalLanguage(['rom', 'ron', 'ro'].includes(code) ? 'rum' : code)
  }).filter(code => knownMarkerCodes.has(code))
  return [...new Set(codes)]
}

export function wrongLanguage(r: SearchResult, variant: BookVariantStatus): boolean {
  const codes = releaseMarkerLanguages(r.title)
  return (codes.length > 0 && (codes.length !== 1 || codes[0] !== variant.language))
    || /release languages? .*(?:variant|requested variant)/i.test(r.rejection ?? '')
}

export function matchesVariantFormat(r: SearchResult, variant: BookVariantStatus, dualFormat: boolean): boolean {
  if (r.mediaType && r.mediaType !== variant.mediaType) return false
  if (!r.mediaType && dualFormat) return false
  // Keep the format vocabulary aligned with formatTokens in internal/indexer/release.go.
  const formats = [...r.title.matchAll(/\b(EPUB|AZW3|AZW|MOBI|PDF|DJVU|CBR|CBZ|FB2|LIT|RTF|TXT|M4B|M4A|FLAC|MP3|OGG)\b/gi)]
    .map(m => /^(M4B|M4A|FLAC|MP3|OGG)$/i.test(m[1]) ? 'audiobook' : 'ebook')
  return formats.every(format => format === variant.mediaType)
}
