// A member's fixed answer format, edited as a list of fields instead of as
// JSON Schema. Only flat objects of simple fields are edited this way; any
// other schema is left to the JSON editor.

export type FieldType = 'string' | 'number' | 'boolean' | 'list'
export interface OutputField { name: string; description: string; type: FieldType; required: boolean }

export const fieldTypes: Array<{ value: FieldType; label: string }> = [
  { value: 'string', label: '文字' },
  { value: 'number', label: '数字' },
  { value: 'boolean', label: '是或否' },
  { value: 'list', label: '多条文字' },
]

type Row = Record<string, unknown>
const record = (value: unknown): Row | undefined => value && typeof value === 'object' && !Array.isArray(value) ? value as Row : undefined
const only = (row: Row, keys: string[]) => Object.keys(row).every((key) => keys.includes(key))

function fieldType(property: Row): FieldType | undefined {
  if (!only(property, ['type', 'description', 'items'])) return undefined
  if (property.type === 'array') {
    const items = record(property.items)
    return items && only(items, ['type']) && items.type === 'string' ? 'list' : undefined
  }
  if (property.items !== undefined) return undefined
  if (property.type === 'string' || property.type === 'boolean') return property.type
  return property.type === 'number' || property.type === 'integer' ? 'number' : undefined
}

/** The fields of a schema the list can edit, or undefined when it cannot. */
export function schemaFields(schema: unknown): OutputField[] | undefined {
  const root = record(schema)
  if (!root || root.type !== 'object' || !only(root, ['type', 'properties', 'required', 'additionalProperties'])) return undefined
  if (root.additionalProperties !== undefined && root.additionalProperties !== false) return undefined
  const properties = record(root.properties) ?? {}
  const required = Array.isArray(root.required) ? root.required : []
  const fields: OutputField[] = []
  for (const [name, value] of Object.entries(properties)) {
    const property = record(value), type = property && fieldType(property)
    if (!property || !type) return undefined
    fields.push({ name, description: typeof property.description === 'string' ? property.description : '', type, required: required.includes(name) })
  }
  return fields
}

/** What stops a field list from being saved, in the page's words. */
export function fieldsProblem(fields: OutputField[]): string | undefined {
  const names = fields.map((field) => field.name.trim())
  if (names.some((name) => !name)) return '每一项都要有名称'
  if (new Set(names).size !== names.length) return '名称不能重复'
  return undefined
}

export function fieldsSchema(fields: OutputField[]): Record<string, unknown> {
  const properties: Row = {}
  for (const field of fields) {
    const description = field.description.trim() ? { description: field.description.trim() } : {}
    properties[field.name.trim()] = field.type === 'list' ? { type: 'array', items: { type: 'string' }, ...description } : { type: field.type, ...description }
  }
  return { type: 'object', properties, required: fields.filter((field) => field.required).map((field) => field.name.trim()), additionalProperties: false }
}
