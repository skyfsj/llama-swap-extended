import sys


def main():
    if "--version" in sys.argv:
        print("fake-vllm 0.0.1")
        return
    print("fake-vllm integration fixture")
