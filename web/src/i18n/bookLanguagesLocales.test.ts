import { describe, expect, it } from 'vitest'

import de from './locales/de.json'
import en from './locales/en.json'
import es from './locales/es.json'
import fr from './locales/fr.json'
import id from './locales/id.json'
import ko from './locales/ko.json'
import nb from './locales/nb.json'
import nl from './locales/nl.json'
import sv from './locales/sv.json'
import tl from './locales/tl.json'

type LocaleBookLanguages = {
  label: string
  english: string
  french: string
  romanian: string
}

type LocaleBundle = {
  bookLanguages?: LocaleBookLanguages
}

const bundles: Record<string, LocaleBundle> = {
  de,
  en,
  es,
  fr,
  id,
  ko,
  nb,
  nl,
  sv,
  tl,
}

const expected: Record<string, LocaleBookLanguages> = {
  de: { label: 'Sprachen', english: 'Englisch', french: 'Französisch', romanian: 'Rumänisch' },
  en: { label: 'Languages', english: 'English', french: 'French', romanian: 'Romanian' },
  es: { label: 'Idiomas', english: 'Inglés', french: 'Francés', romanian: 'Rumano' },
  fr: { label: 'Langues', english: 'Anglais', french: 'Français', romanian: 'Roumain' },
  id: { label: 'Bahasa', english: 'Inggris', french: 'Prancis', romanian: 'Rumania' },
  ko: { label: '언어', english: '영어', french: '프랑스어', romanian: '루마니아어' },
  nb: { label: 'Språk', english: 'Engelsk', french: 'Fransk', romanian: 'Rumensk' },
  nl: { label: 'Talen', english: 'Engels', french: 'Frans', romanian: 'Roemeens' },
  sv: { label: 'Språk', english: 'Engelska', french: 'Franska', romanian: 'Rumänska' },
  tl: { label: 'Mga wika', english: 'Ingles', french: 'Pranses', romanian: 'Rumano' },
}

describe('bookLanguages locale coverage', () => {
  it('defines localized labels in every supported locale file', () => {
    for (const [locale, bundle] of Object.entries(bundles)) {
      expect(bundle.bookLanguages, `${locale}.json is missing bookLanguages`).toEqual(expected[locale])
    }
  })
})
