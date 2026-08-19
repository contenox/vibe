use anyhow::{Result, bail};
use std::collections::BTreeMap;

#[derive(Debug, Clone)]
pub struct Table {
    pub headers: Vec<String>,
    pub rows: Vec<Row>,
}

#[derive(Debug, Clone)]
pub struct Row {
    cells: BTreeMap<String, String>,
}

impl Row {
    pub fn get(&self, column: &str) -> &str {
        self.cells
            .get(column)
            .map(String::as_str)
            .unwrap_or_default()
    }

    pub fn cells(&self) -> &BTreeMap<String, String> {
        &self.cells
    }
}

impl Table {
    pub fn parse(text: &str, headers: &[&str]) -> Result<Table> {
        let lines: Vec<&str> = text.lines().collect();
        let header_index = lines.iter().position(|line| is_header(line, headers));
        let Some(header_index) = header_index else {
            bail!("no header row {:?} in:\n{text}", headers.join(" "));
        };

        let header_line: Vec<char> = lines[header_index].chars().collect();
        let starts = column_starts(&header_line, headers)?;

        let mut rows = Vec::new();
        for line in lines.iter().skip(header_index + 1) {
            if line.trim().is_empty() {
                break;
            }
            let chars: Vec<char> = line.chars().collect();
            let mut cells = BTreeMap::new();
            for (index, header) in headers.iter().enumerate() {
                let from = starts[index].min(chars.len());
                let to = starts
                    .get(index + 1)
                    .copied()
                    .unwrap_or(chars.len())
                    .min(chars.len());
                let cell: String = chars[from..to.max(from)].iter().collect();
                cells.insert((*header).to_string(), cell.trim().to_string());
            }
            rows.push(Row { cells });
        }

        Ok(Table {
            headers: headers.iter().map(|h| (*h).to_string()).collect(),
            rows,
        })
    }

    pub fn empty(headers: &[&str]) -> Table {
        Table {
            headers: headers.iter().map(|h| (*h).to_string()).collect(),
            rows: Vec::new(),
        }
    }

    pub fn is_empty(&self) -> bool {
        self.rows.is_empty()
    }

    pub fn len(&self) -> usize {
        self.rows.len()
    }
}

fn is_header(line: &str, headers: &[&str]) -> bool {
    let fields: Vec<&str> = line.split_whitespace().collect();
    fields == headers
}

fn column_starts(header: &[char], headers: &[&str]) -> Result<Vec<usize>> {
    let mut starts = Vec::with_capacity(headers.len());
    let mut cursor = 0usize;
    for name in headers {
        let wanted: Vec<char> = name.chars().collect();
        let mut found = None;
        let mut probe = cursor;
        while probe + wanted.len() <= header.len() {
            if header[probe..probe + wanted.len()] == wanted[..]
                && (probe == 0 || header[probe - 1].is_whitespace())
            {
                found = Some(probe);
                break;
            }
            probe += 1;
        }
        let Some(found) = found else {
            bail!("column {name:?} is not on the header row");
        };
        starts.push(found);
        cursor = found + wanted.len();
    }
    Ok(starts)
}
