import nearley, { Parser } from "nearley";
import grammar from "./query";

type QueryElement = {
  type: string;
  op?: string;
  expressions?: QueryElement[];
  expression?: QueryElement;
  subquery_var?: moo.Token;
  keyword?: moo.Token;
  converter?: moo.Token;
  value?: moo.Token;
}

interface QueryElementValue {
  pieces: { [key: string]: string };
  start: number;
  len: number;
}

const queryGrammar: nearley.Grammar = nearley.Grammar.fromCompiled(grammar);

export default function analyze(query: string): {
  [key: string]: QueryElementValue;
} {
  const parser: Parser = new nearley.Parser(queryGrammar);
  try {
    parser.feed(query);
  } catch (parseError) {
    console.log(
      `Error at character ${(parseError as { offset: number }).offset}`
    );
    return {};
  }

  const result: { [key: string]: QueryElementValue } = {};
  const elements = [...(parser.results as QueryElement[])];
  for (;;) {
    const elem = elements.pop();
    if (elem === undefined) break;
    if (
      elem.type == "logic" &&
      elem.op == "and" &&
      elem.expressions !== undefined
    ) {
      elements.push(...elem.expressions);
      continue;
    }
    if (elem.type == "expression" || elem.keyword === undefined) continue;
    if (!["sort", "limit", "ltime"].includes(elem.keyword.value)) continue;
    const pieces: { [key: string]: string } = {};
    let start: number | null = null;
    let end: number | null = null;
    for (const [k, v] of Object.entries(elem)) {
      if (k == "type") continue;
      if (v == null) continue;
      const v2 = v as { value: string; col: number; text: string };
      pieces[k] = v2.value;
      if (start == null || start > v2.col) {
        start = v2.col;
      }
      if (end == null || end < v2.col + v2.text.length) {
        end = v2.col + v2.text.length;
      }
    }
    if (start == null || end == null) continue;
    result[elem.keyword.value] = { pieces, start, len: end - start };
  }
  return result;
}
