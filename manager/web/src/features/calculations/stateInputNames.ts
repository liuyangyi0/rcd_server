/** Keep this contract aligned with calculation.validateStateInputs in Go. */
export function isValidStateInputName(name: string): boolean {
  return name.length <= 128
    && /^[A-Za-z0-9_]*[A-Za-z_][A-Za-z0-9_]*$/.test(name)
    && !['if', 'true', 'false'].includes(name.toLowerCase());
}
