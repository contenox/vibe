import { Input } from "./Input";

interface NumberInputProps extends Omit<React.InputHTMLAttributes<HTMLInputElement>, 'onChange' | 'value'> {
  value: number | string;
  onChange: (value: number) => void;
}

export function NumberInput({
  value,
  onChange,
  className,
  ...props
}: NumberInputProps) {
  const handleChange = (e: React.ChangeEvent<HTMLInputElement>) => {
    const numValue = parseFloat(e.target.value);
    if (!isNaN(numValue)) {
      onChange(numValue);
    } else if (e.target.value === "") {
      onChange(0);
    }
  };

  return (
    <Input
      type="number"
      value={value}
      onChange={handleChange}
      className={className}
      {...props}
    />
  );
}
