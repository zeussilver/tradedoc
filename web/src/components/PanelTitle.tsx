// PanelTitle is a numbered section heading. The number is decoration (the
// order is already clear from the page), so screen readers skip it.
export function PanelTitle({ id, step, children }: { id: string; step: number; children: string }) {
  return (
    <h2 id={id}>
      <span className="step" aria-hidden="true">
        {step}
      </span>
      {children}
    </h2>
  )
}
