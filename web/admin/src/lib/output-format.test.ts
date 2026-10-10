import { describe, expect, it } from 'vitest'
import { fieldsProblem, fieldsSchema, schemaFields, type OutputField } from './output-format'

const fields: OutputField[] = [
  { name: '是否跟进', description: '给出结论', type: 'boolean', required: true },
  { name: '预算', description: '', type: 'number', required: false },
  { name: '待确认项', description: '', type: 'list', required: true },
]

describe('answer format as a list of fields', () => {
  it('writes a flat schema and reads it back unchanged', () => {
    const schema = fieldsSchema(fields)
    expect(schema).toEqual({
      type: 'object', additionalProperties: false, required: ['是否跟进', '待确认项'],
      properties: { 是否跟进: { type: 'boolean', description: '给出结论' }, 预算: { type: 'number' }, 待确认项: { type: 'array', items: { type: 'string' } } },
    })
    expect(schemaFields(schema)).toEqual(fields)
  })

  it('reads integers as numbers and an object without fields as an empty list', () => {
    expect(schemaFields({ type: 'object', properties: { 数量: { type: 'integer' } } })).toEqual([{ name: '数量', description: '', type: 'number', required: false }])
    expect(schemaFields({ type: 'object', required: ['summary'] })).toEqual([])
  })

  it('leaves shapes it cannot express to the JSON editor', () => {
    for (const schema of [
      { type: 'array' },
      { type: 'object', properties: { 状态: { type: 'string', enum: ['a', 'b'] } } },
      { type: 'object', properties: { 明细: { type: 'object', properties: {} } } },
      { type: 'object', properties: { 行: { type: 'array', items: { type: 'number' } } } },
      { type: 'object', properties: {}, additionalProperties: true },
      { type: 'object', properties: {}, oneOf: [] },
    ]) expect(schemaFields(schema)).toBeUndefined()
  })

  it('asks for a name on every field and refuses repeated names', () => {
    expect(fieldsProblem(fields)).toBeUndefined()
    expect(fieldsProblem([{ ...fields[0], name: ' ' }])).toBe('每一项都要有名称')
    expect(fieldsProblem([fields[0], { ...fields[1], name: '是否跟进' }])).toBe('名称不能重复')
  })
})
