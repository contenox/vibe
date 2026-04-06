// src/pages/admin/chains/components/ChainJsonEditor.tsx
import Editor, { OnMount } from '@monaco-editor/react';
import type { editor as MonacoEditor } from 'monaco-editor';
import { useEffect, useRef, useState } from 'react';
import { useMonacoAppTheme } from '../../../../lib/monacoAppTheme';
import type { ChainDefinition } from '../../../../lib/types';

interface Props {
  chain: ChainDefinition;
  className?: string;
  /** Bubble the raw JSON string up so the page can parse/save it. */
  onChangeText?: (text: string) => void;
}

export default function ChainJsonEditor({ chain, className, onChangeText }: Props) {
  const [text, setText] = useState('');
  const editorRef = useRef<MonacoEditor.IStandaloneCodeEditor | null>(null);
  const monacoTheme = useMonacoAppTheme();

  useEffect(() => {
    const initial = JSON.stringify(chain, null, 2);
    setText(initial);
    onChangeText?.(initial);
  }, [chain, onChangeText]);

  const handleMount: OnMount = (editor, monaco) => {
    editorRef.current = editor;
    monaco.languages.json.jsonDefaults.setDiagnosticsOptions({
      validate: true,
      allowComments: false,
      schemas: [],
    });
  };

  return (
    <div className={`flex min-h-0 flex-1 rounded-lg border ${className ?? ''}`}>
      <Editor
        height="100%"
        theme={monacoTheme}
        defaultLanguage="json"
        value={text}
        onChange={v => {
          const next = v ?? '';
          setText(next);
          onChangeText?.(next);
        }}
        onMount={handleMount}
        options={{
          automaticLayout: true,
          wordWrap: 'on',
          minimap: { enabled: false },
          tabSize: 2,
          padding: { top: 12, bottom: 12 },
          fontFamily: 'ui-monospace, SFMono-Regular, Menlo, monospace',
          renderWhitespace: 'selection',
          scrollBeyondLastLine: false,
        }}
      />
    </div>
  );
}
