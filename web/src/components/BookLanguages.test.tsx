import { describe, it, expect, vi } from 'vitest'
import { render, screen, fireEvent } from '@testing-library/react'
import { useState } from 'react'
import BookLanguages from './BookLanguages'

vi.mock('react-i18next', () => ({
  useTranslation: () => ({
    t: (key: string) => {
      const strings: Record<string, string> = {
        'bookLanguages.label': 'Languages',
        'bookLanguages.english': 'English',
        'bookLanguages.french': 'French',
        'bookLanguages.romanian': 'Romanian',
      }
      return strings[key] ?? key
    },
  }),
}))

function Harness({ disabled = false }: { disabled?: boolean }) {
  const [value, setValue] = useState<string[]>(['eng'])
  return (
    <>
      <BookLanguages value={value} onChange={setValue} disabled={disabled} />
      <div data-testid="value">{value.join(',')}</div>
    </>
  )
}

describe('BookLanguages', () => {
  it('shows English by default and toggles selected languages in canonical order', () => {
    render(<Harness />)

    const english = screen.getByRole('checkbox', { name: 'English' })
    const french = screen.getByRole('checkbox', { name: 'French' })
    const romanian = screen.getByRole('checkbox', { name: 'Romanian' })

    expect(english).toBeChecked()
    expect(french).not.toBeChecked()
    expect(romanian).not.toBeChecked()
    expect(screen.getByTestId('value')).toHaveTextContent('eng')

    fireEvent.click(romanian)
    expect(screen.getByTestId('value')).toHaveTextContent('eng,rum')

    fireEvent.click(french)
    expect(screen.getByTestId('value')).toHaveTextContent('eng,fre,rum')

    fireEvent.click(english)
    expect(screen.getByTestId('value')).toHaveTextContent('fre,rum')
  })

  it('disables every checkbox while sending', () => {
    render(<Harness disabled />)

    expect(screen.getByRole('checkbox', { name: 'English' })).toBeDisabled()
    expect(screen.getByRole('checkbox', { name: 'French' })).toBeDisabled()
    expect(screen.getByRole('checkbox', { name: 'Romanian' })).toBeDisabled()
  })
})
