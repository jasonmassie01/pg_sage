import { describe, expect, it } from 'vitest'
import { maskConnectionURL, parseConnectionURL } from './connectionUrl'

// ux-04b: a pasted connection URL is parsed in the browser into the
// DatabaseForm fields. No concurrent access tests: pure functions.

describe('parseConnectionURL', () => {
  it('fills every DatabaseForm field from a full URL', () => {
    const got = parseConnectionURL(
      'postgres://sage%40ops:p%40ss%3Aw0rd@orders.c9.us-east-1.rds.amazonaws.com:6543/' +
      'orders?sslmode=verify-full')
    expect(got.error).toBeUndefined()
    expect(got.fields).toEqual({
      name: 'orders', host: 'orders.c9.us-east-1.rds.amazonaws.com', port: 6543,
      database_name: 'orders', username: 'sage@ops', password: 'p@ss:w0rd',
      sslmode: 'verify-full',
    })
  })

  it('accepts postgresql:// and defaults port, sslmode and database like libpq', () => {
    const got = parseConnectionURL('  postgresql://sage_agent@db.internal  ')
    expect(got.fields).toEqual({
      name: 'sage_agent', host: 'db.internal', port: 5432, database_name: 'sage_agent',
      username: 'sage_agent', password: '', sslmode: 'require',
    })
  })

  it('reads a bracketed IPv6 host', () => {
    expect(parseConnectionURL('postgres://u:p@[::1]:5433/app').fields)
      .toMatchObject({ host: '::1', port: 5433, database_name: 'app' })
  })

  it.each([
    ['', 'Paste a connection URL'],
    ['   ', 'Paste a connection URL'],
    ['postgres:///app', 'no host'],
    ['postgres://user:pw@/app', 'no host'],
    ['mysql://u:p@h/app', 'Unknown scheme "mysql:"'],
    ['https://u:p@h/app', 'Unknown scheme "https:"'],
    ['orders.example.com:5432', 'Unknown scheme'],
    ['postgres://u:p@h/app?sslmode=bogus', 'sslmode "bogus"'],
    ['postgres://u:p@h:0/app', 'port'],
    ['postgres://u:p@h:70000/app', 'port'],
    ['postgres://h/app', 'no user'],
    ['not a url at all', 'Unknown scheme'],
  ])('refuses %j with a field error naming %s', (text, want) => {
    const got = parseConnectionURL(text)
    expect(got.fields).toBeUndefined()
    expect(got.error).toContain(want)
  })

  it('never puts the password in an error', () => {
    const got = parseConnectionURL('postgres://u:hunter2@h/app?sslmode=bogus')
    expect(got.error).not.toContain('hunter2')
  })
})

describe('maskConnectionURL', () => {
  it('hides the password and keeps the rest', () => {
    expect(maskConnectionURL('postgres://u:hunter2@h:5432/app?sslmode=require'))
      .toBe('postgres://u:****@h:5432/app?sslmode=require')
  })

  it('leaves a URL without a password as it was', () => {
    expect(maskConnectionURL('postgres://u@h/app')).toBe('postgres://u@h/app')
  })

  it('masks what it cannot parse entirely', () => {
    expect(maskConnectionURL('garbage hunter2')).toBe('')
  })
})
