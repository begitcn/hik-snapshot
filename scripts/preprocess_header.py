#!/usr/bin/env python3
"""
预处理海康 HCNetSDK.h 头文件，修复 C++ 语法使其兼容 C 语言
"""

import re
import sys

def preprocess_hikvision_header(content):
    lines = content.split('\n')
    result_lines = []
    in_function = False
    paren_depth = 0

    for line in lines:
        # 检测是否在函数声明中（以 NET_DVR_API 开头）
        if 'NET_DVR_API' in line:
            in_function = True
            paren_depth = line.count('(') - line.count(')')

        if in_function:
            # 在函数声明中，移除默认参数
            # 只移除函数参数中的默认值，格式: = VALUE 后面跟 , 或 )
            processed = re.sub(r'=\s*NULL(?=[\s,)])', '', line)
            processed = re.sub(r'=\s*TRUE(?=[\s,)])', '', processed)
            processed = re.sub(r'=\s*FALSE(?=[\s,)])', '', processed)
            processed = re.sub(r'=\s*\d+(?=[\s,)])', '', processed)
            processed = re.sub(r'=\s*0x[0-9a-fA-F]+(?=[\s,)])', '', processed)

            # 更新括号深度
            paren_depth += line.count('(') - line.count(')')
            if paren_depth <= 0 and ';' in line:
                in_function = False
                paren_depth = 0
        else:
            # 不在函数中，处理 enum 中的十六进制值
            # 模式: IDENTIFIER xNN, -> IDENTIFIER = 0xNN,
            # 只匹配大写标识符后跟 x 和十六进制数字
            processed = re.sub(r'(\b[A-Z_][A-Z0-9_]*)\s+x([0-9a-fA-F]{1,8})(?=[\s,)])', r'\1 = 0x\2', line)

        result_lines.append(processed)

    content = '\n'.join(result_lines)

    # 1. 移除 extern "C"
    content = re.sub(r'extern\s*"C"', '', content)

    # 2. 移除 __stdcall 和 CALLBACK
    content = re.sub(r'\b__stdcall\b', '', content)
    content = re.sub(r'\bCALLBACK\b', '', content)

    # 3. 删除空的 #define 行
    content = re.sub(r'^\s*#define\s*$', '', content, flags=re.MULTILINE)

    # 4. 修复 enum 类型使用问题
    # 在函数参数中将 "ENUM_NAME var" 改为 "enum ENUM_NAME var"
    # 这些是没有 typedef 的 enum 类型
    enum_types_without_typedef = ['ADDITIONAL_LIB']

    for enum_type in enum_types_without_typedef:
        # 匹配函数参数中的类型使用 (不包括 enum 定义行)
        # 模式: (ENUM_TYPE name) 在参数列表中
        content = re.sub(
            rf'([,(]\s*)({enum_type})(\s+[a-zA-Z_])',
            rf'\1enum \2\3',
            content
        )

    return content

def main():
    if len(sys.argv) != 2:
        print("Usage: preprocess_header.py <header_file>")
        sys.exit(1)

    filepath = sys.argv[1]

    with open(filepath, 'r', encoding='utf-8', errors='ignore') as f:
        content = f.read()

    processed = preprocess_hikvision_header(content)

    with open(filepath, 'w', encoding='utf-8') as f:
        f.write(processed)

    print(f"Preprocessed: {filepath}")

if __name__ == '__main__':
    main()
