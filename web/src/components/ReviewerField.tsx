import { useId } from 'react'

// ReviewerField is the name recorded with every review and sign-off. There
// is no login in this demo, so the name is what the person types.
export function ReviewerField({ value, onChange }: { value: string; onChange: (name: string) => void }) {
  const id = useId()
  return (
    <div className="form-row reviewer">
      <label htmlFor={id}>Reviewer name</label>
      <input
        id={id}
        type="text"
        value={value}
        maxLength={64}
        autoComplete="name"
        onChange={(e) => onChange(e.target.value)}
      />
    </div>
  )
}
