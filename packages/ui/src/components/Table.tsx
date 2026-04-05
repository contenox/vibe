import { cn } from "../utils";

type TableProps = React.HTMLAttributes<HTMLTableElement> & {
  columns: string[];
  children: React.ReactNode;
};

export function Table({ columns, children, className, ...props }: TableProps) {
  return (
    <div className="border-secondary-200 dark:border-dark-secondary-300 overflow-x-auto rounded-lg border">
      <table className={cn("w-full min-w-[600px]", className)} {...props}>
        <thead className="bg-secondary-50 dark:bg-dark-surface-100">
          <tr>
            {columns.map((column) => (
              <th
                key={column}
                className="text-secondary-600 dark:text-dark-secondary-400 px-4 py-3 text-left text-sm font-medium"
              >
                {column}
              </th>
            ))}
          </tr>
        </thead>
        <tbody className="divide-secondary-200 dark:divide-dark-secondary-300 divide-y">
          {children}
        </tbody>
      </table>
    </div>
  );
}

type TableRowProps = React.HTMLAttributes<HTMLTableRowElement>;

export function TableRow({ className, ...props }: TableRowProps) {
  return (
    <tr
      className={cn(
        "hover:bg-secondary-50 dark:hover:bg-dark-surface-100",
        className,
      )}
      {...props}
    />
  );
}

type TableCellProps = React.TdHTMLAttributes<HTMLTableCellElement>;

export function TableCell({ className, ...props }: TableCellProps) {
  return (
    <td
      className={cn(
        "text-secondary-800 dark:text-dark-secondary-200 px-4 py-3 text-sm",
        className,
      )}
      {...props}
    />
  );
}
