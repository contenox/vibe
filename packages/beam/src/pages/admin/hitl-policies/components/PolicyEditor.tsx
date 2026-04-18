import Editor, { OnMount } from '@monaco-editor/react';
import type { editor as MonacoEditor } from 'monaco-editor';
import { useEffect, useRef } from 'react';
import { defineBeamMonacoThemes, useMonacoAppTheme } from '../../../../lib/monacoAppTheme';

interface Props {
  value: string;
  onChange: (text: string) => void;
  className?: string;
}

export default function PolicyEditor({ value, onChange, className }: Props) {
  const editorRef = useRef<MonacoEditor.IStandaloneCodeEditor | null>(null);
  const monacoApiRef = useRef<Parameters<OnMount>[1] | null>(null);
  const monacoTheme = useMonacoAppTheme();

  useEffect(() => {
    const editor = editorRef.current;
    if (editor && editor.getValue() !== value) {
      editor.setValue(value);
    }
  }, [value]);

  useEffect(() => {
    monacoApiRef.current?.editor.setTheme(monacoTheme);
  }, [monacoTheme]);

  const handleMount: OnMount = (editor, monacoApi) => {
    editorRef.current = editor;
    monacoApiRef.current = monacoApi;
    defineBeamMonacoThemes(monacoApi);
    monacoApi.editor.setTheme(monacoTheme);
    editor.onDidChangeModelContent(() => {
      onChange(editor.getValue());
    });
  };

  return (
    <div className={className}>
      <Editor
        height="100%"
        defaultLanguage="json"
        defaultValue={value}
        onMount={handleMount}
        options={{
          minimap: { enabled: false },
          fontSize: 13,
          lineNumbers: 'on',
          wordWrap: 'on',
          formatOnPaste: true,
          formatOnType: true,
        }}
      />
    </div>
  );
}
