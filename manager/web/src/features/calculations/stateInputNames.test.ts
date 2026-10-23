import { expect, it } from 'vitest';
import { isValidStateInputName } from './stateInputNames';

it.each(['STATION', '_STATE', '_', '9CYE91GH201_COM1', '11_STATE', '9_11', '123_', '9'.repeat(127) + 'A'])('accepts expression-compatible state input %s', (name) => {
  expect(isValidStateInputName(name)).toBe(true);
});

it.each(['', '0', '11', '00011', '9'.repeat(128), 'IF', 'if', 'If', 'true', 'TRUE', 'false', 'False', '9CYE-STATE', '9CYE.STATE', '9CYE/STATE', '9CYE STATE', '9CYE\nSTATE', '9CYE状态', '9'.repeat(128) + 'A'])('rejects invalid state input %j', (name) => {
  expect(isValidStateInputName(name)).toBe(false);
});
